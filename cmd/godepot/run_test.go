package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

func runCmd(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, os.Getenv, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCommandLine(t *testing.T) {
	if code, out, _ := runCmd("version"); code != 0 || out != "godepot dev\n" {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, out, _ := runCmd("help"); code != 0 || !strings.Contains(out, "reconcile") {
		t.Fatalf("help: %d %q", code, out)
	}
	if code, _, errs := runCmd(); code != exitUsage || !strings.Contains(errs, "usage:") {
		t.Fatalf("no command: %d %q", code, errs)
	}
	if code, _, errs := runCmd("push"); code != exitUsage || !strings.Contains(errs, `no command "push"`) {
		t.Fatalf("unknown: %d %q", code, errs)
	}
	if code, _, errs := runCmd("validate", "-fix"); code != exitUsage || !strings.Contains(errs, "-fix is for reconcile") {
		t.Fatalf("fix: %d %q", code, errs)
	}
	if code, _, errs := runCmd("validate", "-nope"); code != exitUsage {
		t.Fatalf("flag: %d %q", code, errs)
	}
}

func TestSettingsThatAreWrongStopTheCommand(t *testing.T) {
	for _, k := range []string{"GODEPOT_RQLITE_URL", "GODEPOT_BLOBGATE_URL", "GODEPOT_BLOBGATE_TENANT", "GODEPOT_BLOBGATE_SECRET", "GODEPOT_BLOBGATE_SECRET_FILE"} {
		t.Setenv(k, "")
	}
	code, _, errs := runCmd("validate")
	if code != exitFailed || !strings.Contains(errs, "GODEPOT_RQLITE_URL is required") {
		t.Fatalf("%d %q", code, errs)
	}
}

// With the database and blobgate of tools/test/blobgate-stand.sh: the commands, from outside.
func TestCommandsAgainstTheStand(t *testing.T) {
	if os.Getenv("GODEPOT_BLOBGATE_URL") == "" || os.Getenv("GODEPOT_RQLITE_URL") == "" {
		t.Skip("GODEPOT_BLOBGATE_URL and GODEPOT_RQLITE_URL are not set (tools/test/blobgate-stand.sh)")
	}
	if code, out, errs := runCmd("validate"); code != 0 || out != "settings: ok\ndatabase: ok\nstore: ok\n" {
		t.Fatalf("validate: %d %q %q", code, out, errs)
	}
	if code, out, errs := runCmd("migrate"); code != 0 || !strings.Contains(out, "the schema of the database is at version") {
		t.Fatalf("migrate: %d %q %q", code, out, errs)
	}
	if code, out, errs := runCmd("check"); code != 0 || !strings.HasPrefix(out, "ok: the store") {
		t.Fatalf("check: %d %q %q", code, out, errs)
	}
	if code, out, errs := runCmd("reconcile"); code != 0 || !strings.Contains(out, "dry: nothing was changed") {
		t.Fatalf("reconcile: %d %q %q", code, out, errs)
	}
	if code, out, _ := runCmd("reconcile", "-fix"); code != 0 || strings.Contains(out, "dry") {
		t.Fatalf("reconcile -fix: %d %q", code, out)
	}
	t.Setenv("GODEPOT_BLOBGATE_STORE", "no-such-store")
	if code, _, errs := runCmd("check"); code != exitFailed || errs == "" {
		t.Fatalf("a store that is not there: %d %q", code, errs)
	}
}

// Two replicas of the background work: one holds the lease, the other waits; the one that lets go is replaced at once.
func TestOnlyOneReplicaDoesTheBackgroundWork(t *testing.T) {
	url := os.Getenv("GODEPOT_RQLITE_URL")
	if url == "" {
		t.Skip("GODEPOT_RQLITE_URL is not set")
	}
	cfg, err := loadConfig(envOf(map[string]string{"GODEPOT_RQLITE_URL": url, "GODEPOT_BLOBGATE_URL": "http://127.0.0.1:1", "GODEPOT_BLOBGATE_TENANT": "t", "GODEPOT_BLOBGATE_SECRET": "a-secret-of-16-chars"}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, b := newApp(cfg, os.Stdout, os.Stderr), newApp(cfg, os.Stdout, os.Stderr)
	if err := a.start(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(ctx, meta.S("DELETE FROM lease")); err != nil {
		t.Fatal(err)
	}
	a.pass(ctx)
	b.pass(ctx)
	if a.m.leader.Value() != 1 || b.m.leader.Value() != 0 || a.m.passes.Value("") != 1 || b.m.passes.Value("") != 0 {
		t.Fatalf("a: leader %d passes %d; b: leader %d passes %d", a.m.leader.Value(), a.m.passes.Value(""), b.m.leader.Value(), b.m.passes.Value(""))
	}
	if err := a.lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	b.pass(ctx)
	if b.m.leader.Value() != 1 || b.m.passes.Value("") != 1 {
		t.Fatalf("b did not take over: leader %d", b.m.leader.Value())
	}
	if err := b.lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func standEnv(t *testing.T) {
	t.Helper()
	if os.Getenv("GODEPOT_BLOBGATE_URL") == "" || os.Getenv("GODEPOT_RQLITE_URL") == "" {
		t.Skip("GODEPOT_BLOBGATE_URL and GODEPOT_RQLITE_URL are not set (tools/test/blobgate-stand.sh)")
	}
}

// serve: answers /healthz, /readyz and /metrics, does the background work as the leader, and on the end of its context lets go of the lease and returns.
func TestServe(t *testing.T) {
	standEnv(t)
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port, cfg.MetricsPort, cfg.CollectEvery = 18150, 18151, time.Second
	a := newApp(cfg, os.Stdout, os.Stderr)
	if err := a.start(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(context.Background(), meta.S("DELETE FROM lease")); err != nil { // what an earlier test left
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.serve(ctx) }()

	get := func(url string) (int, string) {
		for i := 0; i < 50; i++ {
			resp, err := http.Get(url)
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return resp.StatusCode, string(b)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s does not answer", url)
		return 0, ""
	}
	if c, b := get("http://127.0.0.1:18150/healthz"); c != 200 || b != "ok\n" {
		t.Fatalf("healthz: %d %q", c, b)
	}
	if c, b := get("http://127.0.0.1:18150/readyz"); c != 200 || !strings.Contains(b, `"status":"ready"`) {
		t.Fatalf("readyz: %d %q", c, b)
	}
	deadline := time.Now().Add(15 * time.Second)
	for a.m.leader.Value() != 1 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if _, b := get("http://127.0.0.1:18151/metrics"); !strings.Contains(b, "\ngodepot_up 1\n") || !strings.Contains(b, "\ngodepot_leader 1\n") {
		t.Fatalf("metrics: %s", b)
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not end")
	}
	if a.m.up.Value() != 0 || a.m.leader.Value() != 0 {
		t.Fatalf("up %d leader %d after the end", a.m.up.Value(), a.m.leader.Value())
	}
	rows, err := a.db.Query(context.Background(), meta.S(`SELECT until FROM lease WHERE name = 'background' AND holder = ?`, a.lease.Holder))
	if err != nil || len(rows) != 1 || meta.Int(rows[0][0]) != 0 {
		t.Fatalf("the lease was not let go: %v %v", rows, err)
	}
}

func TestServeStopsWhenAPortIsTaken(t *testing.T) {
	standEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:18160")
	if err != nil {
		t.Skip("the port is in use here")
	}
	defer ln.Close()
	cfg, _ := loadConfig(os.Getenv)
	cfg.Port = 18160
	a := newApp(cfg, os.Stdout, os.Stderr)
	done := make(chan error, 1)
	go func() { done <- a.serve(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "18160") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve went on with a port that is taken")
	}
}

// reconcile exits with 3 and names the blob when a live blob has no bytes.
func TestReconcileSaysWhenDataIsLost(t *testing.T) {
	standEnv(t)
	cfg, _ := loadConfig(os.Getenv)
	a := newApp(cfg, os.Stdout, os.Stderr)
	ctx := context.Background()
	if err := a.start(ctx, false); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("ab", 32)
	if _, err := a.db.Exec(ctx, meta.S(`INSERT INTO blobs (digest, size, key, state, unref_since) VALUES (?, 5, ?, 'live', 1)`, digest, "blobs/sha256/ab/"+digest+"/0000000000000000")); err != nil {
		t.Fatal(err)
	}
	defer a.db.Exec(ctx, meta.S(`DELETE FROM blobs WHERE digest = ?`, digest))
	code, out, _ := runCmd("reconcile")
	if code != exitMissing || !strings.Contains(out, "MISSING: 1 live blobs") || !strings.Contains(out, digest) {
		t.Fatalf("%d %q", code, out)
	}
}
