// Package proxytest tests the proxy and group repositories of the formats raw and oci (docs/proxy.md, step 5 of docs/plan.md) against false upstreams: a server of the test that is slow, that dies in
// the middle of a body, that answers 429 or 500, that changes bytes. Only tests are here.
package proxytest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/gomod"
	"github.com/zystem/godepot/internal/maven"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/metrics"
	"github.com/zystem/godepot/internal/npm"
	"github.com/zystem/godepot/internal/oci"
	"github.com/zystem/godepot/internal/pypi"
	"github.com/zystem/godepot/internal/raw"
	"github.com/zystem/godepot/internal/server"
	"github.com/zystem/godepot/internal/settings"
	"github.com/zystem/godepot/internal/testutil"
	"github.com/zystem/godepot/internal/upstream"
)

// fake is a false upstream: a server of the test that counts what it is asked.
type fake struct {
	*httptest.Server
	hits atomic.Int64
	mu   sync.Mutex
	seen []string
	auth []string // the Authorization headers it saw
}

func newFake(t *testing.T, h http.HandlerFunc) *fake {
	t.Helper()
	f := &fake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.RequestURI())
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) count() int { return int(f.hits.Load()) }

// asked says how many requests had this method and path.
func (f *fake) asked(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.seen {
		if s == method+" "+path {
			n++
		}
	}
	return n
}

type rig struct {
	t       *testing.T
	ts      *httptest.Server
	db      meta.DB
	layer   *blob.Layer
	store   *testutil.MemStore
	pool    *upstream.Pool
	clock   *atomic.Int64
	admin   string
	stale   *metrics.Counter
	changed *metrics.Counter
	reqs    *metrics.Counter
	errs    *metrics.Counter
	gate    *format.Gate
}

func newRig(t *testing.T, settingsJSON string) *rig {
	t.Helper()
	db := testutil.DB(t)
	clock := &atomic.Int64{}
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	store := testutil.NewMemStore()
	layer := &blob.Layer{DB: db, Store: store, Now: now, Grace: time.Hour, UploadTTL: time.Hour}
	al := &audit.Log{DB: db, Now: now}
	as := &auth.Store{DB: db, Now: now, MaxLife: 200 * time.Hour}
	set, err := settings.Parse(strings.NewReader(settingsJSON))
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.New()
	r := &rig{t: t, db: db, layer: layer, store: store, clock: clock,
		stale:   reg.Counter("stale", "x", "repo"),
		changed: reg.Counter("changed", "x", "repo"),
		reqs:    reg.Counter("reqs", "x", "repo"),
		errs:    reg.Counter("errs", "x", "repo"),
		gate:    format.NewGate(64, time.Second),
	}
	r.pool = &upstream.Pool{DB: db, Now: now, M: &upstream.Metrics{Requests: r.reqs, Errors: r.errs, Stale: r.stale, Changed: r.changed}}
	rawFormat := &raw.Raw{Up: r.pool, Blobs: layer, DB: db, Audit: al, Now: now, Downloads: r.gate}
	s := &server.Server{Auth: as, Audit: al, Formats: map[string]format.Format{
		"raw":   rawFormat,
		"go":    gomod.New(rawFormat),
		"pypi":  pypi.New(rawFormat),
		"npm":   npm.New(rawFormat),
		"maven": maven.New(rawFormat),
		"oci":   &oci.OCI{Up: r.pool, Blobs: layer, DB: db, Audit: al, Now: now, Downloads: r.gate, MaxManifest: 1 << 20},
	}}
	s.Settings.Store(set)
	r.ts = httptest.NewServer(s)
	t.Cleanup(r.ts.Close)
	ctx := context.Background()
	if err := as.CreateAccount(ctx, "root", "user", true); err != nil {
		t.Fatal(err)
	}
	all := auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read, auth.Write, auth.Delete, auth.Promote, auth.Admin}}
	r.admin, _, err = as.CreateToken(ctx, "root", []auth.Grant{all}, 100*time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) advance(d time.Duration) { r.clock.Add(int64(d / time.Second)) }

type reply struct {
	code int
	hd   http.Header
	body []byte
	err  error // a body that was cut
}

func (r *rig) do(method, path string, body io.Reader, headers ...string) reply {
	r.t.Helper()
	req, err := http.NewRequest(method, r.ts.URL+path, body)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.admin)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, rerr := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header, b, rerr}
}

func (r *rig) get(path string, headers ...string) reply {
	r.t.Helper()
	return r.do("GET", path, nil, headers...)
}

func (r *rig) expect(a reply, code int) reply {
	r.t.Helper()
	if a.code != code {
		r.t.Fatalf("status %d, want %d: %s", a.code, code, a.body)
	}
	return a
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// auditCount says how many rows of the audit log have the action (and a detail that has the word).
func (r *rig) auditCount(action, word string) int {
	r.t.Helper()
	rows, err := r.db.Query(context.Background(), meta.S(`SELECT detail FROM audit WHERE action = ?`, action))
	if err != nil {
		r.t.Fatal(err)
	}
	n := 0
	for _, row := range rows {
		if strings.Contains(meta.Str(row[0]), word) {
			n++
		}
	}
	return n
}

func conf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// repos makes a settings file from the repositories (JSON objects).
func repos(list ...string) string {
	return `{"repositories": [` + strings.Join(list, ",") + `], "grants": []}`
}

func blobBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) + seed
	}
	return b
}

var _ = bytes.NewReader
