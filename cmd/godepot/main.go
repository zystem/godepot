// godepot is a repository manager on top of blobgate. This release has the blob layer and the background work (docs/blob-layer.md); no format is served yet (docs/spec.md, docs/plan.md).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/blobgate"
	"github.com/zystem/godepot/internal/lease"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/metrics"
)

var version = "dev"

const usage = `usage: godepot <command>

  serve        run: migrate the database, do the background work while holding the lease, answer /healthz and /readyz
  validate     check the settings, and that the database and blobgate can be used (writes nothing that stays)
  migrate      bring the schema of the database to the one this program writes, and stop
  check        try the store: a write of unknown length, a link, a range, a listing, a delete
  reconcile    compare the store with the database; with -fix, remove the keys that nothing names
  version      print the version

The settings are environment variables; docs/configuration.md says each one.
`

func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr)) }

// Exit codes: 0 done, 1 failed, 2 wrong use, 3 reconcile found live blobs whose bytes are gone.
const (
	exitFailed  = 1
	exitUsage   = 2
	exitMissing = 3
)

func run(args []string, env func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, "godepot", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "serve", "validate", "migrate", "check", "reconcile":
	default:
		fmt.Fprintf(stderr, "godepot: no command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fix := fs.Bool("fix", false, "reconcile: remove the keys that nothing names (without it nothing is removed)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *fix && cmd != "reconcile" {
		fmt.Fprintln(stderr, "godepot: -fix is for reconcile")
		return exitUsage
	}
	cfg, err := loadConfig(env)
	if err != nil {
		fmt.Fprintf(stderr, "godepot: the settings are not right:\n%s\n", indent(err.Error()))
		return exitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a := newApp(cfg, stdout, stderr)
	var rerr error
	switch cmd {
	case "validate":
		rerr = a.validate(ctx)
	case "migrate":
		if rerr = a.start(ctx, false); rerr == nil {
			fmt.Fprintf(stdout, "the schema of the database is at version %d\n", meta.Version)
		}
	case "check":
		rerr = a.store.Check(ctx)
		if rerr == nil {
			fmt.Fprintf(stdout, "ok: the store %q of tenant %q can be written, linked, read in ranges, listed and deleted\n", cfg.Store, cfg.Tenant)
		}
	case "reconcile":
		if rerr = a.start(ctx, false); rerr == nil {
			return a.reconcile(ctx, !*fix)
		}
	case "serve":
		rerr = a.serve(ctx)
	}
	if rerr != nil {
		fmt.Fprintf(stderr, "godepot %s: %v\n", cmd, rerr)
		return exitFailed
	}
	return 0
}

func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

type app struct {
	cfg    *Config
	db     *meta.Rqlite
	store  *blobgate.Store
	layer  *blob.Layer
	lease  *lease.Lease
	m      *appMetrics
	reg    *metrics.Registry
	out    io.Writer
	errOut io.Writer
	log    *slog.Logger
}

type appMetrics struct {
	passes, passErrors, collected *metrics.Counter
	leader, up                    *metrics.Gauge
}

func newApp(cfg *Config, out, errOut io.Writer) *app {
	reg := metrics.New()
	a := &app{cfg: cfg, reg: reg, out: out, errOut: errOut, log: slog.New(slog.NewJSONHandler(errOut, nil))}
	a.m = &appMetrics{
		passes:     reg.Counter("godepot_collect_passes_total", "Passes of the collector that this replica made while holding the lease.", ""),
		passErrors: reg.Counter("godepot_collect_errors_total", "Passes of the collector that ended with an error.", ""),
		collected:  reg.Counter("godepot_collected_total", "Things the collector removed, by kind: graves (keys deleted from the store), uploads (swept), blobs (nothing named them).", "kind"),
		leader:     reg.Gauge("godepot_leader", "1 while this replica holds the lease of the background work, else 0."),
		up:         reg.Gauge("godepot_up", "1 while the program is running."),
	}
	a.db = meta.NewRqlite(cfg.RqliteURL)
	a.store = blobgate.New(cfg.BlobgateURL, cfg.Tenant, cfg.Secret, cfg.Store)
	a.layer = &blob.Layer{DB: a.db, Store: a.store, Grace: cfg.Grace, UploadTTL: cfg.UploadTTL}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	host, _ := os.Hostname()
	a.lease = &lease.Lease{DB: a.db, Name: "background", Holder: host + "-" + hex.EncodeToString(rnd[:]), TTL: cfg.LeaseTTL}
	return a
}

// waitDB waits for the database for up to two minutes: in a cluster it often starts after the program.
func (a *app) waitDB(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := a.db.Query(ctx, meta.S("SELECT 1"))
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the database does not answer: %w", err)
		}
		a.log.Info("waiting for the database", "error", err.Error())
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// start waits for the database, brings its schema up to date and, with checkStore, tries the store.
func (a *app) start(ctx context.Context, checkStore bool) error {
	if err := a.waitDB(ctx); err != nil {
		return err
	}
	if err := meta.Migrate(ctx, a.db); err != nil {
		return err
	}
	if checkStore {
		if err := a.store.Check(ctx); err != nil {
			return fmt.Errorf("the store cannot be used: %w", err)
		}
	}
	return nil
}

func (a *app) validate(ctx context.Context) error {
	fmt.Fprintln(a.out, "settings: ok")
	if _, err := a.db.Query(ctx, meta.S("SELECT 1")); err != nil {
		return fmt.Errorf("the database does not answer: %w", err)
	}
	fmt.Fprintln(a.out, "database: ok")
	if err := a.store.Check(ctx); err != nil {
		return fmt.Errorf("the store cannot be used: %w", err)
	}
	fmt.Fprintln(a.out, "store: ok")
	return nil
}

func (a *app) reconcile(ctx context.Context, dry bool) int {
	rep, err := a.layer.Reconcile(ctx, dry)
	if err != nil {
		fmt.Fprintf(a.errOut, "godepot reconcile: %v\n", err)
		return exitFailed
	}
	mode := "removed"
	if dry {
		mode = "would remove (dry: nothing was changed, use -fix)"
	}
	fmt.Fprintf(a.out, "checked %d live blobs\n%s %d keys that nothing names\n", rep.Checked, mode, rep.Strays)
	if len(rep.Missing) > 0 {
		fmt.Fprintf(a.out, "MISSING: %d live blobs have no bytes in the store (data is lost; restore the store). The first:\n", len(rep.Missing))
		for i, d := range rep.Missing {
			if i == 20 {
				fmt.Fprintf(a.out, "  ... and %d more\n", len(rep.Missing)-20)
				break
			}
			fmt.Fprintf(a.out, "  %s\n", d)
		}
		return exitMissing
	}
	return 0
}

func (a *app) serve(ctx context.Context) error {
	if err := a.start(ctx, true); err != nil {
		return err
	}
	a.m.up.Set(1)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if _, err := a.db.Query(c, meta.S("SELECT 1")); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"checks":{"database":"fail"},"status":"not ready"}`)
			return
		}
		fmt.Fprint(w, `{"checks":{"database":"ok"},"status":"ready"}`)
	})
	servers := []*http.Server{{Addr: fmt.Sprintf(":%d", a.cfg.Port), Handler: mux, ReadHeaderTimeout: 10 * time.Second}}
	if a.cfg.MetricsPort != 0 {
		mm := http.NewServeMux()
		mm.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			a.reg.Render(w)
		})
		servers = append(servers, &http.Server{Addr: fmt.Sprintf(":%d", a.cfg.MetricsPort), Handler: mm, ReadHeaderTimeout: 10 * time.Second})
	}
	failed := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("serving %s: %w", s.Addr, err)
			}
		}()
	}
	a.log.Info("serving", "version", version, "port", a.cfg.Port, "metrics_port", a.cfg.MetricsPort, "holder", a.lease.Holder)

	// The background work stops when the context ends, and also when a server fails: a replica whose port is taken must end, not go on with no way to be asked whether it is well.
	bgCtx, endBackground := context.WithCancel(ctx)
	defer endBackground()
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); a.background(bgCtx) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-failed:
	}
	endBackground()
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(stopCtx)
	}
	<-loopDone
	a.m.up.Set(0)
	return err
}

// background does the work of the replica that holds the lease: every CollectEvery, a pass of the collector (and more at once while a pass finds a full batch).
func (a *app) background(ctx context.Context) {
	tick := time.NewTicker(a.cfg.CollectEvery)
	defer tick.Stop()
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := a.lease.Release(c); err != nil {
			a.log.Warn("the lease was not released", "error", err.Error())
		}
		a.m.leader.Set(0)
	}()
	for {
		a.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *app) pass(ctx context.Context) {
	held, err := a.lease.Acquire(ctx)
	if err != nil {
		a.m.leader.Set(0)
		if ctx.Err() == nil {
			a.log.Warn("the lease cannot be asked for", "error", err.Error())
		}
		return
	}
	if !held {
		a.m.leader.Set(0)
		return
	}
	a.m.leader.Set(1)
	for i := 0; i < 20 && ctx.Err() == nil; i++ { // a full batch means there is more: go on at once, but give the lease a turn
		got, err := a.layer.Collect(ctx, a.cfg.CollectBatch)
		a.m.passes.Add("", 1)
		a.m.collected.Add("graves", int64(got.Graves))
		a.m.collected.Add("uploads", int64(got.Uploads))
		a.m.collected.Add("blobs", int64(got.Blobs))
		if err != nil {
			a.m.passErrors.Add("", 1)
			if ctx.Err() == nil {
				a.log.Error("a pass of the collector failed", "error", err.Error())
			}
			return
		}
		if got.Graves+got.Uploads+got.Blobs > 0 {
			a.log.Info("collected", "graves", got.Graves, "uploads", got.Uploads, "blobs", got.Blobs)
		}
		if got.Graves < a.cfg.CollectBatch && got.Uploads < a.cfg.CollectBatch && got.Blobs < a.cfg.CollectBatch {
			return
		}
		if ok, err := a.lease.Acquire(ctx); err != nil || !ok {
			return
		}
	}
}
