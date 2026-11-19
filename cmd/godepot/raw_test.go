package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/testutil"
)

const rawSettings = `{
 "repositories": [{"name": "files", "format": "raw", "kind": "hosted"}],
 "grants": [{"account": "ci-build", "repositories": ["files"], "actions": ["read", "write"]}]
}`

// The whole program with a real rqlite and a real blobgate: the accounts and the token are made by the commands, the file is written and read by HTTP, and the settings are read again on SIGHUP.
func TestRawThroughTheProgram(t *testing.T) {
	standEnv(t)
	ctx := context.Background()
	if err := testutil.Reset(ctx, mustDB(t)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "godepot.json")
	if err := os.WriteFile(file, []byte(rawSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODEPOT_CONFIG_FILE", file)
	t.Setenv("GODEPOT_TOKEN_MAX_DAYS", "7")

	if code, out, errs := runCmd("validate"); code != 0 || !strings.Contains(out, "settings file: ok (1 repositories, 1 grants)") {
		t.Fatalf("validate: %d %q %q", code, out, errs)
	}
	if code, out, _ := runCmd("new-account", "ci-build", "-robot"); code != 0 || !strings.Contains(out, "account ci-build (robot) made") {
		t.Fatalf("new-account: %d %q", code, out)
	}
	if code, _, errs := runCmd("new-account", "ci-build"); code != exitFailed || !strings.Contains(errs, "exists") {
		t.Fatalf("a second account of the name: %d %q", code, errs)
	}
	if code, _, _ := runCmd("new-account", "ci$x"); code != exitFailed {
		t.Fatalf("a name with $: %d", code)
	}
	if code, _, _ := runCmd("new-account"); code != exitUsage {
		t.Fatalf("no name: %d", code)
	}
	if code, _, errs := runCmd("validate", "extra"); code != exitUsage || !strings.Contains(errs, "takes no arguments") {
		t.Fatalf("an argument: %d %q", code, errs)
	}
	// a token needs a life, a scope, and a life that is not longer than the longest
	for _, args := range [][]string{
		{"new-token", "ci-build", "-repositories", "files", "-actions", "read"},
		{"new-token", "ci-build", "-life", "1h", "-actions", "read"},
		{"new-token", "ci-build", "-life", "1h", "-repositories", "files"},
		{"new-token", "ci-build", "-life", "200h", "-repositories", "files", "-actions", "read"},
		{"new-token", "nobody", "-life", "1h", "-repositories", "files", "-actions", "read"},
	} {
		if code, out, _ := runCmd(args...); code != exitFailed || strings.Contains(out, "gd_") {
			t.Errorf("%v: %d %q", args, code, out)
		}
	}
	code, out, errs := runCmd("new-token", "ci-build", "-life", "2h", "-repositories", "files", "-actions", "read,write", "-note", "for the test")
	tok := strings.TrimSpace(out)
	if code != 0 || !strings.HasPrefix(tok, "gd_") || strings.Contains(errs, tok) || !strings.Contains(errs, "only time") {
		t.Fatalf("new-token: %d %q %q", code, out, errs)
	}

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port, cfg.MetricsPort, cfg.CollectEvery = 18170, 18171, time.Second
	a := newApp(cfg, os.Stdout, &bytes.Buffer{}) // the log is not wanted in the output of the test
	sctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.serve(sctx) }()
	base := "http://127.0.0.1:18170"
	call := func(method, path, token string, body io.Reader, hdr ...string) (int, []byte) {
		t.Helper()
		var resp *http.Response
		for i := 0; i < 50; i++ {
			req, _ := http.NewRequest(method, base+path, body)
			if token != "" {
				req.SetBasicAuth("x", token)
			}
			for j := 0; j+1 < len(hdr); j += 2 {
				req.Header.Set(hdr[j], hdr[j+1])
			}
			if resp, err = http.DefaultClient.Do(req); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// a file bigger than one block of blobgate, written and read back by range
	data := make([]byte, 20<<20+123)
	_, _ = rand.Read(data)
	if c, b := call("PUT", "/repository/files/big/data.bin", tok, bytes.NewReader(data)); c != 201 {
		t.Fatalf("put: %d %s", c, b)
	}
	if c, b := call("GET", "/repository/files/big/data.bin", tok, nil); c != 200 || !bytes.Equal(b, data) {
		t.Fatalf("get: %d, %d bytes of %d", c, len(b), len(data))
	}
	if c, b := call("GET", "/repository/files/big/data.bin", tok, nil, "Range", "bytes=10000000-10000099"); c != 206 || !bytes.Equal(b, data[10000000:10000100]) {
		t.Fatalf("range: %d", c)
	}
	if c, _ := call("GET", "/repository/files/big/data.bin", "", nil); c != 401 {
		t.Fatalf("no token: %d", c)
	}
	if c, _ := call("GET", "/repository/docs/x", tok, nil); c != 404 {
		t.Fatalf("a repository that is not in the file: %d", c)
	}

	// the settings are read again on SIGHUP: a new repository appears; a file that is not right is refused and the old settings stay
	if err := os.WriteFile(file, []byte(strings.Replace(rawSettings, `[{"name": "files"`, `[{"name": "docs", "format": "raw", "kind": "hosted"}, {"name": "files"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if ok() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s did not happen", what)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	waitFor("the new repository", func() bool { c, _ := call("GET", "/repository/docs/x", tok, nil); return c == 403 })
	if err := os.WriteFile(file, []byte(`{"repositories": [{"name": "files", "format": "nonsense", "kind": "hosted"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(500 * time.Millisecond)
	if c, _ := call("GET", "/repository/files/big/data.bin", tok, nil, "Range", "bytes=0-9"); c != 206 {
		t.Fatalf("after a settings file that is not right the settings in use stay: %d", c)
	}

	if c, b := call("GET", "/repository/files/", tok, nil); c != 200 || !strings.Contains(string(b), "big/data.bin") {
		t.Fatalf("list: %d %s", c, b)
	}
	if c, b := call("GET", "/", "", nil); c == 0 {
		t.Fatalf("%s", b)
	}
	m := &bytes.Buffer{}
	a.reg.Render(m)
	for _, want := range []string{`godepot_writes_total{repo="files"} 1`, `godepot_refused_total{reason="unauthenticated"}`} {
		if !strings.Contains(m.String(), want) {
			t.Errorf("the metrics lack %s:\n%s", want, m)
		}
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
}

func mustDB(t *testing.T) *rqliteDB {
	t.Helper()
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	return newApp(cfg, io.Discard, io.Discard).db
}

type rqliteDB = meta.Rqlite
