package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/metrics"
	"github.com/zystem/godepot/internal/raw"
	"github.com/zystem/godepot/internal/server"
	"github.com/zystem/godepot/internal/settings"
	"github.com/zystem/godepot/internal/testutil"
)

const settingsText = `{
 "repositories": [
  {"name": "files", "format": "raw", "kind": "hosted", "overwrite_paths": ["latest/**"]},
  {"name": "small", "format": "raw", "kind": "hosted", "quota_bytes": 20, "quota_components": 2},
  {"name": "public", "format": "raw", "kind": "hosted", "anonymous_read": true}
 ],
 "grants": [
  {"account": "ci-build", "repositories": ["files"], "paths": ["releases/**"], "actions": ["read", "write", "delete"]},
  {"account": "reader", "repositories": ["files", "public"], "actions": ["read"]}
 ]
}`

// rig is the whole door over a real rqlite and a store in memory, with a log the test can read.
type rig struct {
	t      *testing.T
	ts     *httptest.Server
	db     meta.DB
	auth   *auth.Store
	layer  *blob.Layer
	store  *testutil.MemStore
	log    *bytes.Buffer
	clock  *atomic.Int64
	admin  string // the secret of a token of an admin account with every action
	refuse *metrics.Counter
	srv    *server.Server
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db := testutil.DB(t)
	clock := &atomic.Int64{}
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	store := testutil.NewMemStore()
	layer := &blob.Layer{DB: db, Store: store, Now: now, Grace: time.Hour, UploadTTL: time.Hour}
	logbuf := &bytes.Buffer{}
	lg := slog.New(slog.NewJSONHandler(logbuf, nil))
	al := &audit.Log{DB: db, Log: lg, Now: now}
	as := &auth.Store{DB: db, Now: now, MaxLife: 24 * time.Hour}
	reg := metrics.New()
	refused := reg.Counter("godepot_refused_total", "refusals", "reason")
	set, err := settings.Parse(strings.NewReader(settingsText))
	if err != nil {
		t.Fatal(err)
	}
	s := &server.Server{Auth: as, Audit: al, Refused: refused, Formats: map[string]format.Format{
		"raw": &raw.Raw{Blobs: layer, DB: db, Audit: al, Now: now},
	}}
	s.Settings.Store(set)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	r := &rig{t: t, ts: ts, db: db, auth: as, layer: layer, store: store, log: logbuf, clock: clock, refuse: refused, srv: s}
	ctx := context.Background()
	for _, a := range []struct {
		name, kind string
		admin      bool
	}{{"root", "user", true}, {"ci-build", "robot", false}, {"reader", "user", false}} {
		if err := as.CreateAccount(ctx, a.name, a.kind, a.admin); err != nil {
			t.Fatal(err)
		}
	}
	r.admin = r.token("root", time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read, auth.Write, auth.Delete, auth.Promote, auth.Admin}})
	return r
}

func (r *rig) token(account string, life time.Duration, scope ...auth.Grant) string {
	r.t.Helper()
	tok, _, err := r.auth.CreateToken(context.Background(), account, scope, life, "")
	if err != nil {
		r.t.Fatal(err)
	}
	return tok
}

// do sends a request with a bearer token ("" for none) and returns the status, the headers and the body.
func (r *rig) do(method, path, token string, body io.Reader, headers ...string) (int, http.Header, []byte) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.ts.URL+path, body)
	if err != nil {
		r.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func (r *rig) want(code int, method, path, token string, body string, headers ...string) (http.Header, []byte) {
	r.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	got, h, b := r.do(method, path, token, rd, headers...)
	if got != code {
		r.t.Fatalf("%s %s: status %d, want %d: %s", method, path, got, code, b)
	}
	return h, b
}

func (r *rig) auditRows(action string) []audit.Row {
	r.t.Helper()
	rows, err := (&audit.Log{DB: r.db}).List(context.Background(), 0, 1000, action, "")
	if err != nil {
		r.t.Fatal(err)
	}
	return rows
}

func jsonOf(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%v in %s", err, b)
	}
}

func sum(s string) string { return blob.SumHex([]byte(s)) }

func TestRawEndToEnd(t *testing.T) {
	r := newRig(t)
	ci := r.token("ci-build", 10*time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read, auth.Write, auth.Delete}})
	body := "0123456789abcdef"

	// write, read back
	h, b := r.want(201, "PUT", "/repository/files/releases/1.0/app.tgz", ci, body, "Content-Type", "application/gzip", "X-Godepot-Property-Build", "42", "X-Godepot-Property-Arch", "arm64")
	if h.Get("X-Checksum-Sha256") != sum(body) {
		t.Fatalf("the digest in the answer: %v", h)
	}
	var put struct {
		Path, Sha256 string
		Size         int64
	}
	jsonOf(t, b, &put)
	if put.Path != "releases/1.0/app.tgz" || put.Size != int64(len(body)) {
		t.Fatalf("%+v", put)
	}
	h, b = r.want(200, "GET", "/repository/files/releases/1.0/app.tgz", ci, "")
	if string(b) != body || h.Get("Content-Type") != "application/gzip" || h.Get("ETag") != `"`+sum(body)+`"` {
		t.Fatalf("read: %q %v", b, h)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("a file that a user uploaded must be served as a download: %v", h)
	}
	if h.Get("X-Godepot-Property-Build") != "42" {
		t.Fatalf("properties: %v", h)
	}

	// HEAD, ranges, a conditional read
	h, b = r.want(200, "HEAD", "/repository/files/releases/1.0/app.tgz", ci, "")
	if len(b) != 0 || h.Get("Content-Length") != "16" {
		t.Fatalf("head: %v %q", h, b)
	}
	for _, c := range []struct{ rng, want, cr string }{
		{"bytes=2-5", "2345", "bytes 2-5/16"},
		{"bytes=10-", "abcdef", "bytes 10-15/16"},
		{"bytes=-3", "def", "bytes 13-15/16"},
		{"bytes=0-0", "0", "bytes 0-0/16"},
		{"bytes=4-999", "456789abcdef", "bytes 4-15/16"},
	} {
		h, b = r.want(206, "GET", "/repository/files/releases/1.0/app.tgz", ci, "", "Range", c.rng)
		if string(b) != c.want || h.Get("Content-Range") != c.cr {
			t.Errorf("%s: %q %q", c.rng, b, h.Get("Content-Range"))
		}
	}
	r.want(416, "GET", "/repository/files/releases/1.0/app.tgz", ci, "", "Range", "bytes=16-")
	h, _ = r.want(200, "GET", "/repository/files/releases/1.0/app.tgz", ci, "", "Range", "bytes=0-1,4-5") // several ranges: the whole
	if h.Get("Content-Length") != "16" {
		t.Fatalf("%v", h)
	}
	r.want(304, "GET", "/repository/files/releases/1.0/app.tgz", ci, "", "If-None-Match", `"`+sum(body)+`"`)

	// the same bytes again are fine; other bytes are not (R4)
	r.want(200, "PUT", "/repository/files/releases/1.0/app.tgz", ci, body)
	_, b = r.want(409, "PUT", "/repository/files/releases/1.0/app.tgz", ci, "other bytes")
	if !strings.Contains(string(b), sum(body)) {
		t.Fatalf("the refusal names what is there: %s", b)
	}
	r.want(409, "PUT", "/repository/files/releases/1.0/app.tgz", ci, "x", "X-Checksum-Sha256", sum("x")) // refused before the body is read
	_, b = r.want(200, "GET", "/repository/files/releases/1.0/app.tgz", ci, "")
	if string(b) != body {
		t.Fatalf("an immutable file changed: %q", b)
	}

	// a wrong digest named by the client
	r.want(400, "PUT", "/repository/files/releases/1.0/new", ci, "abc", "X-Checksum-Sha256", sum("not abc"))
	r.want(400, "PUT", "/repository/files/releases/1.0/new", ci, "abc", "X-Checksum-Sha256", "zz")
	r.want(404, "GET", "/repository/files/releases/1.0/new", ci, "")
	r.want(201, "PUT", "/repository/files/releases/1.0/new", ci, "abc", "X-Checksum-Sha256", strings.ToUpper(sum("abc")))

	// a repository that overwrites some paths
	admin := r.admin
	r.want(201, "PUT", "/repository/files/latest/info", admin, "one")
	r.want(200, "PUT", "/repository/files/latest/info", admin, "two", "X-Godepot-Property-Note", "second")
	_, b = r.want(200, "GET", "/repository/files/latest/info", admin, "")
	if string(b) != "two" {
		t.Fatalf("overwrite: %q", b)
	}

	// listing, with a page and a property
	_, b = r.want(200, "GET", "/repository/files/releases/", ci, "")
	var list struct {
		Items []struct {
			Path       string
			Size       int64
			SHA256     string
			Properties map[string]string
		}
		Next string
	}
	list.Items, list.Next = nil, ""
	jsonOf(t, b, &list)
	if len(list.Items) != 2 || list.Items[0].Path != "releases/1.0/app.tgz" || list.Items[1].Path != "releases/1.0/new" || list.Items[0].Properties["arch"] != "arm64" {
		t.Fatalf("%+v", list)
	}
	_, b = r.want(200, "GET", "/repository/files/releases/?limit=1", ci, "")
	list.Items, list.Next = nil, ""
	jsonOf(t, b, &list)
	if len(list.Items) != 1 || list.Next != "releases/1.0/app.tgz" {
		t.Fatalf("a page: %+v", list)
	}
	_, b = r.want(200, "GET", "/repository/files/releases/?after="+list.Next, ci, "")
	list.Items, list.Next = nil, ""
	jsonOf(t, b, &list)
	if len(list.Items) != 1 || list.Items[0].Path != "releases/1.0/new" || list.Next != "" {
		t.Fatalf("the next page: %+v", list)
	}
	_, b = r.want(200, "GET", "/repository/files/releases/?property=build:42", ci, "")
	list.Items, list.Next = nil, ""
	jsonOf(t, b, &list)
	if len(list.Items) != 1 || list.Items[0].Path != "releases/1.0/app.tgz" {
		t.Fatalf("by property: %+v", list)
	}
	r.want(400, "GET", "/repository/files/releases/?limit=0", ci, "")
	r.want(400, "GET", "/repository/files/releases/?property=nocolon", ci, "")

	// a delete; the blob is not unreferenced for good until the grace time is over
	r.want(204, "DELETE", "/repository/files/releases/1.0/new", ci, "")
	r.want(404, "DELETE", "/repository/files/releases/1.0/new", ci, "")
	r.want(404, "GET", "/repository/files/releases/1.0/new", ci, "")
	before := r.store.Keys()
	r.clock.Add(7200)
	if _, err := r.layer.Collect(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if after := r.store.Keys(); after >= before {
		t.Fatalf("the bytes of a deleted file are kept for ever: %d keys before, %d after", before, after)
	}
	r.want(200, "GET", "/repository/files/releases/1.0/app.tgz", ci, "") // what is still named is still there

	// every change has its line in the audit log
	puts := r.auditRows("put")
	dels := r.auditRows("delete")
	if len(puts) < 6 || len(dels) != 1 || dels[0].Actor != "ci-build" || dels[0].Path != "releases/1.0/new" || dels[0].Result != audit.OK {
		t.Fatalf("audit: %d puts, deletes %+v", len(puts), dels)
	}
	if !strings.Contains(r.log.String(), `"action":"delete"`) {
		t.Fatal("the audit line is not in the log")
	}
}

func TestPathsAreChecked(t *testing.T) {
	r := newRig(t)
	for _, p := range []string{"a//b", "a/./b", "a/../b", "a/b/", "x%00y", "a%5Cb"} {
		got, _, _ := r.do("PUT", "/repository/files/"+p, r.admin, strings.NewReader("x"))
		if got != 400 && got != 301 && got != 307 {
			t.Errorf("%q: %d", p, got)
		}
	}
	if raw.CheckPath(strings.Repeat("a", 2000)) == nil {
		t.Error("a path that is too long")
	}
	if raw.CheckPath("fine/path.tar.gz") != nil || raw.CheckPath("ünï/ço") != nil {
		t.Error("an ordinary path is refused")
	}
}

func TestAccess(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	narrow := r.token("ci-build", time.Hour, auth.Grant{Repositories: []string{"files"}, Paths: []string{"releases/**"}, Actions: []string{auth.Read, auth.Write}})
	reader := r.token("reader", time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read}})
	r.want(201, "PUT", "/repository/files/releases/x", narrow, "x")

	// no token: 401 that asks for one
	h, _ := r.want(401, "GET", "/repository/files/releases/x", "", "")
	if !strings.Contains(h.Get("Www-Authenticate"), "Basic") {
		t.Fatalf("%v", h)
	}
	// a token that is not good: 401, and the audit line says why, and does not say the secret
	r.want(401, "GET", "/repository/files/releases/x", "gd_000000000000_"+strings.Repeat("a", 43), "")
	r.want(401, "GET", "/repository/files/releases/x", "garbage", "")
	if rows := r.auditRows("auth"); len(rows) != 2 || rows[0].Result != audit.Refused {
		t.Fatalf("%+v", rows)
	}
	// a token out of scope: 403, audited with who it was
	r.want(403, "PUT", "/repository/files/other/x", narrow, "x")
	r.want(403, "DELETE", "/repository/files/releases/x", narrow, "")
	r.want(403, "PUT", "/repository/files/releases/y", reader, "x")
	r.want(403, "PUT", "/repository/public/x", narrow, "x")
	rows := r.auditRows("write")
	found := false
	for _, x := range rows {
		if x.Actor == "reader" && x.Result == audit.Refused && x.Path == "releases/y" && x.Repo == "files" && x.Token != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal of the reader is not in the audit log: %+v", rows)
	}
	if r.refuse.Value("forbidden") < 4 || r.refuse.Value("unauthenticated") < 3 {
		t.Fatalf("the refusals are not counted: %d %d", r.refuse.Value("forbidden"), r.refuse.Value("unauthenticated"))
	}
	// a reader may read; the account decides, the token only narrows
	r.want(200, "GET", "/repository/files/releases/x", reader, "")
	wide := r.token("reader", time.Hour, auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Admin}})
	r.want(403, "PUT", "/repository/files/releases/z", wide, "x") // the token says admin, the account says read
	// anonymous read of a repository that allows it
	r.want(201, "PUT", "/repository/public/readme", r.admin, "hello")
	_, b := r.want(200, "GET", "/repository/public/readme", "", "")
	if string(b) != "hello" {
		t.Fatalf("%q", b)
	}
	r.want(401, "PUT", "/repository/public/readme2", "", "x")
	// basic authentication with the token as the password, any name
	req, _ := http.NewRequest("GET", r.ts.URL+"/repository/files/releases/x", nil)
	req.SetBasicAuth("whatever", reader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("basic: %v %v", resp, err)
	}
	resp.Body.Close()
	req.Header.Set("Authorization", "Digest x")
	if resp, _ = http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("a scheme that is not known: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// an unknown repository, a format with no door
	r.want(404, "GET", "/repository/nothing/x", r.admin, "")
	// an account that is disabled loses every token at once
	if err := r.auth.SetDisabled(ctx, "reader", true); err != nil {
		t.Fatal(err)
	}
	r.want(401, "GET", "/repository/files/releases/x", reader, "")
}

func TestQuota(t *testing.T) {
	r := newRig(t)
	a := r.admin
	r.want(201, "PUT", "/repository/small/a", a, strings.Repeat("a", 12))
	_, b := r.want(413, "PUT", "/repository/small/b", a, strings.Repeat("b", 12)) // 24 bytes > 20
	if !strings.Contains(string(b), `"quota_bytes":20`) {
		t.Fatalf("the refusal has the figures: %s", b)
	}
	r.want(413, "PUT", "/repository/small/huge", a, strings.Repeat("c", 21))
	r.want(404, "GET", "/repository/small/b", a, "")
	r.want(201, "PUT", "/repository/small/c", a, "cc") // 14 bytes, 2 components
	r.want(413, "PUT", "/repository/small/d", a, "dd") // a third component
	r.want(204, "DELETE", "/repository/small/a", a, "")
	r.want(201, "PUT", "/repository/small/d", a, strings.Repeat("d", 18)) // 20 bytes
	if rows := r.auditRows("put"); rows[0].Result != audit.OK || len(rows) < 6 {
		t.Fatalf("%+v", rows)
	}
	// a refused write leaves nothing behind: no row, and its bytes are swept
	var n int
	rows, _ := r.db.Query(context.Background(), meta.S(`SELECT COUNT(*) FROM assets WHERE repo = 'small'`))
	n = int(meta.Int(rows[0][0]))
	if n != 2 {
		t.Fatalf("%d assets", n)
	}
	rows, _ = r.db.Query(context.Background(), meta.S(`SELECT COUNT(*) FROM uploads`))
	if meta.Int(rows[0][0]) != 0 {
		t.Fatal("an upload is left")
	}
}

func TestWritersRace(t *testing.T) {
	r := newRig(t)
	// 12 writers of different bytes to one path of a repository that does not overwrite: one wins, the rest are told 409, and the file is the winner's
	type res struct {
		code int
		body string
	}
	out := make(chan res, 12)
	for i := 0; i < 12; i++ {
		body := strings.Repeat(string(rune('a'+i)), 10)
		go func() {
			code, _, _ := r.do("PUT", "/repository/files/releases/race", r.admin, strings.NewReader(body))
			out <- res{code, body}
		}()
	}
	var winner string
	created := 0
	for i := 0; i < 12; i++ {
		x := <-out
		switch x.code {
		case 201:
			created++
			winner = x.body
		case 409:
		default:
			t.Fatalf("status %d", x.code)
		}
	}
	if created != 1 {
		t.Fatalf("%d writers won", created)
	}
	_, b := r.want(200, "GET", "/repository/files/releases/race", r.admin, "")
	if string(b) != winner {
		t.Fatalf("the file is %q, the winner wrote %q", b, winner)
	}
	// and in a repository that overwrites, the last write stands and nothing is left unnamed
	for i := 0; i < 12; i++ {
		body := strings.Repeat(string(rune('A'+i)), 10)
		go func() {
			code, _, _ := r.do("PUT", "/repository/files/latest/race", r.admin, strings.NewReader(body))
			out <- res{code, body}
		}()
	}
	for i := 0; i < 12; i++ {
		if x := <-out; x.code != 201 && x.code != 200 {
			t.Fatalf("status %d", x.code)
		}
	}
	rows, _ := r.db.Query(context.Background(), meta.S(`SELECT COUNT(*) FROM assets WHERE path = 'latest/race'`))
	if meta.Int(rows[0][0]) != 1 {
		t.Fatalf("%d assets at one path", meta.Int(rows[0][0]))
	}
	// every ref belongs to an asset that is there
	rows, _ = r.db.Query(context.Background(), meta.S(`SELECT COUNT(*) FROM refs WHERE owner LIKE 'asset:%' AND substr(owner, 7) NOT IN (SELECT id FROM assets)`))
	if meta.Int(rows[0][0]) != 0 {
		t.Fatalf("%d references of assets that are gone", meta.Int(rows[0][0]))
	}
}
