// godepot is a repository manager on top of blobgate. This release has the blob layer and the background work (docs/blob-layer.md), accounts and tokens, and one format, raw (docs/raw.md); docs/spec.md
// and docs/plan.md say what comes next.
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

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/blobgate"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/gomod"
	"github.com/zystem/godepot/internal/lease"
	"github.com/zystem/godepot/internal/maven"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/metrics"
	"github.com/zystem/godepot/internal/npm"
	"github.com/zystem/godepot/internal/oci"
	"github.com/zystem/godepot/internal/pypi"
	"github.com/zystem/godepot/internal/raw"
	"github.com/zystem/godepot/internal/server"
	"github.com/zystem/godepot/internal/settings"
	"github.com/zystem/godepot/internal/upstream"
)

var version = "dev"

const usage = `usage: godepot <command>

  serve        run: migrate the database, serve the repositories and the API, do the background work while holding the lease
  validate     check the settings and the settings file, and that the database and blobgate can be used (writes nothing that stays)
  new-account  make an account: new-account NAME [-robot] [-admin]
  new-token    make a token and print it once: new-token ACCOUNT -life 720h -repositories files -actions read,write [-paths 'releases/**']
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
	case "serve", "validate", "migrate", "check", "reconcile", "new-account", "new-token":
	default:
		fmt.Fprintf(stderr, "godepot: no command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fix := fs.Bool("fix", false, "reconcile: remove the keys that nothing names (without it nothing is removed)")
	robot := fs.Bool("robot", false, "new-account: a robot (an account for a program), not a user")
	admin := fs.Bool("admin", false, "new-account: an admin account (every action on every repository; its tokens still have their own scope)")
	life := fs.Duration("life", 0, "new-token: how long the token lives, like 720h (required; at most GODEPOT_TOKEN_MAX_DAYS)")
	repos := fs.String("repositories", "", "new-token: the repositories (or patterns) the token is for, separated by commas")
	paths := fs.String("paths", "", "new-token: the paths (patterns) the token is for, separated by commas; none is every path")
	acts := fs.String("actions", "", "new-token: what the token may do: read, write, delete, promote, admin, separated by commas")
	note := fs.String("note", "", "new-token: a note for the list of tokens")
	// flags may follow the name, as in `new-account ci-build -robot`
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return exitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if *fix && cmd != "reconcile" {
		fmt.Fprintln(stderr, "godepot: -fix is for reconcile")
		return exitUsage
	}
	if cmd != "new-account" && cmd != "new-token" && len(rest) > 0 {
		fmt.Fprintf(stderr, "godepot: %s takes no arguments (it has %q)\n", cmd, rest[0])
		return exitUsage
	}
	if (cmd == "new-account" || cmd == "new-token") && len(rest) != 1 {
		fmt.Fprintf(stderr, "godepot: %s takes the name of one account\n\n%s", cmd, usage)
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
	case "new-account":
		if rerr = a.start(ctx, false); rerr == nil {
			kind := "user"
			if *robot {
				kind = "robot"
			}
			if rerr = a.auth.CreateAccount(ctx, rest[0], kind, *admin); rerr == nil {
				a.audit.Record(ctx, audit.Event{Actor: "cli", Action: "account.create", Result: audit.OK, Detail: rest[0] + " " + kind})
				fmt.Fprintf(stdout, "account %s (%s) made\n", rest[0], kind)
			}
		}
	case "new-token":
		if rerr = a.start(ctx, false); rerr == nil {
			rerr = a.newToken(ctx, rest[0], *life, splitList(*repos), splitList(*paths), splitList(*acts), *note)
		}
	}
	if rerr != nil {
		fmt.Fprintf(stderr, "godepot %s: %v\n", cmd, rerr)
		return exitFailed
	}
	return 0
}

func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

type app struct {
	cfg    *Config
	auth   *auth.Store
	audit  *audit.Log
	srv    *server.Server
	db     *meta.Rqlite
	store  *blobgate.Store
	layer  *blob.Layer
	oci    *oci.OCI
	lease  *lease.Lease
	m      *appMetrics
	reg    *metrics.Registry
	out    io.Writer
	errOut io.Writer
	log    *slog.Logger
}

type appMetrics struct {
	passes, passErrors, collected *metrics.Counter
	reads, writes, deletes        *metrics.Counter
	refused                       *metrics.Counter
	upstream                      *upstream.Metrics
	leader, up, active            *metrics.Gauge
}

func newApp(cfg *Config, out, errOut io.Writer) *app {
	reg := metrics.New()
	a := &app{cfg: cfg, reg: reg, out: out, errOut: errOut, log: slog.New(slog.NewJSONHandler(errOut, nil))}
	a.m = &appMetrics{
		passes:     reg.Counter("godepot_collect_passes_total", "Passes of the collector that this replica made while holding the lease.", ""),
		passErrors: reg.Counter("godepot_collect_errors_total", "Passes of the collector that ended with an error.", ""),
		collected:  reg.Counter("godepot_collected_total", "Things the collector removed, by kind: graves (keys deleted from the store), uploads (swept), blobs (nothing named them).", "kind"),
		reads:      reg.Counter("godepot_reads_total", "Files read, by repository.", "repo"),
		writes:     reg.Counter("godepot_writes_total", "Files written (new or changed), by repository.", "repo"),
		deletes:    reg.Counter("godepot_deletes_total", "Files deleted, by repository.", "repo"),
		refused:    reg.Counter("godepot_refused_total", "Requests refused, by reason: unauthenticated, forbidden, conflict (a file is immutable), quota, digest (the bytes were not the ones named).", "reason"),
		upstream: &upstream.Metrics{
			Requests: reg.Counter("godepot_upstream_requests_total", "Requests sent to upstreams of proxy repositories, by repository.", "repo"),
			Errors:   reg.Counter("godepot_upstream_errors_total", "Requests to upstreams that ended in an error, a 429 or a 5xx (each starts or lengthens a pause), and requests not sent because the upstream was paused, by repository.", "repo"),
			Stale:    reg.Counter("godepot_upstream_stale_total", "Answers served from the cache, marked stale, because the upstream could not be asked, by repository.", "repo"),
			Changed:  reg.Counter("godepot_upstream_changed_total", "Times an upstream gave other bytes for what cannot change (a digest, an immutable path), by repository: the cached bytes stay.", "repo"),
		},
		leader: reg.Gauge("godepot_leader", "1 while this replica holds the lease of the background work, else 0."),
		up:     reg.Gauge("godepot_up", "1 while the program is running."),
		active: reg.Gauge("godepot_downloads_active", "Files and blobs being sent to clients now (about 10 MiB of blobgate's memory each); the most is GODEPOT_MAX_DOWNLOADS."),
	}
	a.db = meta.NewRqlite(cfg.RqliteURL)
	a.store = blobgate.New(cfg.BlobgateURL, cfg.Tenant, cfg.Secret, cfg.Store)
	a.layer = &blob.Layer{DB: a.db, Store: a.store, Grace: cfg.Grace, UploadTTL: cfg.UploadTTL}
	a.auth = &auth.Store{DB: a.db, MaxLife: cfg.TokenMaxLife}
	a.audit = &audit.Log{DB: a.db, Log: a.log}
	gate := format.NewGate(cfg.MaxDownloads, 5*time.Second) // one for every door: what limits is the memory of blobgate
	pool := &upstream.Pool{DB: a.db, M: a.m.upstream}
	a.oci = &oci.OCI{Up: pool, Blobs: a.layer, DB: a.db, Audit: a.audit, Reads: a.m.reads, Writes: a.m.writes, Deletes: a.m.deletes, Refused: a.m.refused, Active: a.m.active,
		Downloads: gate, Rate: int64(cfg.MinRate), UploadTTL: cfg.UploadTTL}
	rawFormat := &raw.Raw{Up: pool, Blobs: a.layer, DB: a.db, Audit: a.audit, Reads: a.m.reads, Writes: a.m.writes, Deletes: a.m.deletes, Refused: a.m.refused,
		Downloads: gate, Rate: int64(cfg.MinRate), Active: a.m.active}
	a.srv = &server.Server{Auth: a.auth, Audit: a.audit, Refused: a.m.refused, Formats: map[string]format.Format{
		"raw":   rawFormat,
		"go":    gomod.New(rawFormat),
		"pypi":  pypi.New(rawFormat),
		"npm":   npm.New(rawFormat),
		"maven": maven.New(rawFormat),
		"oci":   a.oci,
	}}
	a.srv.Settings.Store(&settings.Settings{})
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

// loadSettings reads the settings file (if there is one) and gives it to the server.
func (a *app) loadSettings() error {
	if a.cfg.ConfigFile == "" {
		return nil
	}
	set, err := settings.Load(a.cfg.ConfigFile)
	if err != nil {
		return err
	}
	a.srv.Settings.Store(set)
	return nil
}

func (a *app) newToken(ctx context.Context, account string, life time.Duration, repos, paths, acts []string, note string) error {
	tok, info, err := a.auth.CreateToken(ctx, account, []auth.Grant{{Repositories: repos, Paths: paths, Actions: acts}}, life, note)
	if err != nil {
		return err
	}
	a.audit.Record(ctx, audit.Event{Actor: "cli", Action: "token.create", Result: audit.OK, Detail: info.ID + " for " + account})
	fmt.Fprintf(a.errOut, "token %s for %s, valid until %s. This is the only time it is shown; it is not kept.\n", info.ID, account, time.Unix(info.Expires, 0).UTC().Format(time.RFC3339))
	fmt.Fprintln(a.out, tok)
	return nil
}

func (a *app) validate(ctx context.Context) error {
	fmt.Fprintln(a.out, "settings: ok")
	if a.cfg.ConfigFile != "" {
		if err := a.loadSettings(); err != nil {
			return err
		}
		set := a.srv.Settings.Load()
		fmt.Fprintf(a.out, "settings file: ok (%d repositories, %d grants)\n", len(set.Repositories), len(set.Grants))
	}
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
	if err := a.loadSettings(); err != nil {
		return err
	}
	if err := a.start(ctx, true); err != nil {
		return err
	}
	a.m.up.Set(1)
	// SIGHUP reads the settings file again; a file that is not right is refused and the settings in use stay.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := a.loadSettings(); err != nil {
					a.log.Error("the settings file was not read again; the settings in use stay", "error", err.Error())
				} else {
					a.log.Info("the settings file was read again")
				}
			}
		}
	}()
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
	door := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if server.Handles(r.URL.Path) { // before the mux, which would send a path with "//" in it to another address
			a.srv.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	servers := []*http.Server{{Addr: fmt.Sprintf(":%d", a.cfg.Port), Handler: door, ReadHeaderTimeout: 10 * time.Second}}
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
	if n, err := a.oci.Sweep(ctx, a.cfg.CollectBatch); err != nil {
		a.m.passErrors.Add("", 1)
		if ctx.Err() == nil {
			a.log.Error("the sweep of OCI uploads failed", "error", err.Error())
		}
	} else if n > 0 {
		a.m.collected.Add("oci_uploads", int64(n))
		a.log.Info("collected", "oci_uploads", n)
	}
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
