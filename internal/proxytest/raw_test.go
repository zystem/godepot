package proxytest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

// proxyRaw is a proxy repository "up" of the format raw whose upstream is f, with the settings of the upstream that the test adds (JSON members, with a leading comma, or "").
func proxyRaw(f *fake, more string) string {
	return repos(conf(`{"name": "up", "format": "raw", "kind": "proxy", "upstream": {"url": %q, "allow_private": true%s}}`, f.URL, more))
}

func fileServer(files map[string][]byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"`+sum(b)[:16]+`"`)
		if r.Header.Get("If-None-Match") == `"`+sum(b)[:16]+`"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(b)
	}
}

// P1, P3: a file is fetched when it is first asked for, is sent to the client, and is kept; the next ask does not reach the upstream.
func TestRawProxyFetchesOnceAndKeeps(t *testing.T) {
	data := blobBytes(100000, 1)
	f := newFake(t, fileServer(map[string][]byte{"/lib/a.bin": data}))
	r := newRig(t, proxyRaw(f, ""))
	a := r.expect(r.get("/repository/up/lib/a.bin"), 200)
	if !bytes.Equal(a.body, data) || a.hd.Get("Content-Type") != "text/plain" {
		t.Fatalf("the first answer: %v, %d bytes", a.hd, len(a.body))
	}
	for i := 0; i < 3; i++ {
		b := r.expect(r.get("/repository/up/lib/a.bin"), 200)
		if !bytes.Equal(b.body, data) || b.hd.Get("X-Checksum-Sha256") != sum(data) {
			t.Fatalf("the cached answer: %v", b.hd)
		}
	}
	if f.count() != 1 {
		t.Fatalf("the upstream was asked %d times, want 1", f.count())
	}
	if r.auditCount("fetch", sum(data)) != 1 {
		t.Fatal("the fetch is not in the audit log")
	}
	// A proxy is read-only.
	r.expect(r.do("PUT", "/repository/up/x", bytes.NewReader([]byte("x"))), 405)
	r.expect(r.do("DELETE", "/repository/up/lib/a.bin", nil), 405)
	// A range from the cache.
	p := r.expect(r.get("/repository/up/lib/a.bin", "Range", "bytes=10-19"), 206)
	if !bytes.Equal(p.body, data[10:20]) {
		t.Fatal("range")
	}
}

// P2, T15: 100 clients ask one cold file: one fetch from the upstream, and every client gets the bytes.
func TestRawProxyJoinsFetches(t *testing.T) {
	data := blobBytes(300000, 2)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // long enough that all 100 are waiting
		_, _ = w.Write(data)
	})
	r := newRig(t, proxyRaw(f, ""))
	var wg sync.WaitGroup
	bad := make(chan string, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := r.do("GET", "/repository/up/big.bin", nil)
			if a.code != 200 || !bytes.Equal(a.body, data) || a.err != nil {
				bad <- strconv.Itoa(a.code)
			}
		}()
	}
	wg.Wait()
	close(bad)
	for b := range bad {
		t.Fatalf("a client got %s", b)
	}
	if f.count() != 1 {
		t.Fatalf("100 clients made %d fetches, want 1", f.count())
	}
}

// T16, P1: the first bytes reach the client while the upstream has not sent the rest.
func TestRawProxyStreamsWhileFetching(t *testing.T) {
	first := blobBytes(200000, 3)
	rest := blobBytes(200000, 4)
	release := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(first)+len(rest)))
		_, _ = w.Write(first)
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write(rest)
	})
	r := newRig(t, proxyRaw(f, ""))
	req, _ := http.NewRequest("GET", r.ts.URL+"/repository/up/slow.bin", nil)
	req.Header.Set("Authorization", "Bearer "+r.admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make([]byte, 100000)
	if _, err := io.ReadFull(resp.Body, got); err != nil { // this returns only if bytes came while the upstream is still holding the rest
		t.Fatal(err)
	}
	if !bytes.Equal(got, first[:100000]) {
		t.Fatal("the first bytes are not the first bytes of the upstream")
	}
	close(release)
	all, err := io.ReadAll(resp.Body)
	if err != nil || len(all) != len(first)+len(rest)-100000 {
		t.Fatalf("the rest: %d bytes, %v", len(all), err)
	}
	// And the whole was kept: asked again, it is the cache.
	b := r.expect(r.get("/repository/up/slow.bin"), 200)
	if !bytes.Equal(b.body, append(append([]byte{}, first...), rest...)) || f.count() != 1 {
		t.Fatalf("the kept file: %d bytes, %d asks", len(b.body), f.count())
	}
}

// An upstream that dies in the middle of a body: the client is not given a short body as if it were whole, nothing is kept, the upstream is paused; and after the pause the file is fetched.
func TestRawProxyUpstreamDiesInTheBody(t *testing.T) {
	data := blobBytes(100000, 5)
	var dies = true
	var mu sync.Mutex
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		d := dies
		mu.Unlock()
		if !d {
			_, _ = w.Write(data)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data[:40000])
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	r := newRig(t, proxyRaw(f, ""))
	a := r.do("GET", "/repository/up/dies.bin", nil)
	if a.err == nil && a.code == 200 && len(a.body) == len(data) {
		t.Fatal("the client was given a whole body")
	}
	if a.err == nil && a.code == 200 {
		t.Fatalf("the client was given a short body as a good answer: %d bytes", len(a.body))
	}
	rows, _ := r.db.Query(context.Background(), meta.S(`SELECT 1 FROM assets WHERE repo = 'up'`))
	if len(rows) != 0 {
		t.Fatal("a cut file was kept")
	}
	// The upstream is paused: the next ask does not reach it.
	n := f.count()
	b := r.do("GET", "/repository/up/dies.bin", nil)
	if b.code != 503 || b.hd.Get("Retry-After") == "" || f.count() != n {
		t.Fatalf("during the pause: %d, Retry-After %q, asks %d -> %d", b.code, b.hd.Get("Retry-After"), n, f.count())
	}
	mu.Lock()
	dies = false
	mu.Unlock()
	r.advance(10 * time.Second)
	c := r.expect(r.get("/repository/up/dies.bin"), 200)
	if !bytes.Equal(c.body, data) {
		t.Fatal("after the pause the file is not the whole")
	}
}

// P4, T13: an error of the upstream is never remembered as "not found"; it pauses the upstream for a growing time, and a 429 is paused for the time the upstream names.
func TestRawProxyErrorsPauseAndAreNeverNotFound(t *testing.T) {
	status := 500
	retry := ""
	var mu sync.Mutex
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		s, ra := status, retry
		mu.Unlock()
		if s != 200 {
			if ra != "" {
				w.Header().Set("Retry-After", ra)
			}
			w.WriteHeader(s)
			return
		}
		_, _ = w.Write([]byte("here"))
	})
	r := newRig(t, proxyRaw(f, ""))
	a := r.do("GET", "/repository/up/f", nil)
	if a.code != 503 || a.hd.Get("Retry-After") == "" {
		t.Fatalf("500: %d %v", a.code, a.hd)
	}
	if n := f.count(); n != 1 {
		t.Fatal(n)
	}
	// Paused: not asked.
	for i := 0; i < 5; i++ {
		if b := r.do("GET", "/repository/up/f", nil); b.code != 503 {
			t.Fatalf("paused: %d", b.code)
		}
	}
	if f.count() != 1 {
		t.Fatalf("a paused upstream was asked: %d", f.count())
	}
	// After 6 seconds it is asked again, fails again, and the pause is longer (10 s): at 6 s more it is still paused.
	r.advance(6 * time.Second)
	r.do("GET", "/repository/up/f", nil)
	if f.count() != 2 {
		t.Fatalf("after the pause: %d asks", f.count())
	}
	r.advance(6 * time.Second)
	if b := r.do("GET", "/repository/up/f", nil); b.code != 503 || f.count() != 2 {
		t.Fatalf("the second pause is longer than 6 s: %d, %d asks", b.code, f.count())
	}
	// Not "not found": when the upstream is well again the file is there.
	mu.Lock()
	status = 200
	mu.Unlock()
	r.advance(15 * time.Second)
	r.expect(r.get("/repository/up/f"), 200)

	// 429 with Retry-After: the pause is what the upstream says, and the client is told it.
	mu.Lock()
	status, retry = 429, "120"
	mu.Unlock()
	b := r.do("GET", "/repository/up/g", nil)
	if b.code != 503 || b.hd.Get("Retry-After") != "120" {
		t.Fatalf("429: %d, Retry-After %q", b.code, b.hd.Get("Retry-After"))
	}
	n := f.count()
	r.advance(100 * time.Second)
	if c := r.do("GET", "/repository/up/g", nil); c.code != 503 || f.count() != n {
		t.Fatal("the upstream was asked before the time it named")
	}
	// Even a 429 forever is paused for at most 5 minutes.
	mu.Lock()
	retry = "86400"
	mu.Unlock()
	r.advance(30 * time.Second)
	r.do("GET", "/repository/up/g", nil)
	rows, _ := r.db.Query(context.Background(), meta.S(`SELECT pause_until FROM upstream_state WHERE repo = 'up'`))
	if left := meta.Int(rows[0][0]) - r.clock.Load(); left > 300 {
		t.Fatalf("a pause of %d seconds: the most is 300", left)
	}
	if r.errs.Value("up") < 3 || r.reqs.Value("up") < 3 {
		t.Fatalf("the metrics: requests %d, errors %d", r.reqs.Value("up"), r.errs.Value("up"))
	}
}

// P4: a "not found" is remembered for a short time, per name, and the API clears it.
func TestRawProxyRemembersNotFoundForAWhile(t *testing.T) {
	have := false
	var mu sync.Mutex
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := have
		mu.Unlock()
		if h {
			_, _ = w.Write([]byte("now here"))
			return
		}
		http.NotFound(w, r)
	})
	r := newRig(t, proxyRaw(f, ""))
	for i := 0; i < 4; i++ {
		r.expect(r.get("/repository/up/new.txt"), 404)
	}
	if f.count() != 1 {
		t.Fatalf("a missing name was asked %d times", f.count())
	}
	// Published upstream a minute ago: still remembered (10 minutes)...
	mu.Lock()
	have = true
	mu.Unlock()
	r.advance(time.Minute)
	r.expect(r.get("/repository/up/new.txt"), 404)
	// ...until the API clears it.
	a := r.do("POST", "/api/v1/repositories/up/forget?name=raw:new.txt", nil)
	r.expect(a, 200)
	r.expect(r.get("/repository/up/new.txt"), 200)
	// And it ends by itself after the time: 10 minutes.
	mu.Lock()
	have = false
	mu.Unlock()
	r.expect(r.get("/repository/up/other.txt"), 404)
	mu.Lock()
	have = true
	mu.Unlock()
	r.advance(11 * time.Minute)
	r.expect(r.get("/repository/up/other.txt"), 200)
	// Not a proxy: no such call.
	r.expect(r.do("POST", "/api/v1/repositories/nothing/forget", nil), 404)
}

// P3: what can change has a time to live and is asked with If-None-Match; when the upstream cannot answer, the cached file is served, marked stale.
func TestRawProxyServesStaleWhenTheUpstreamIsDown(t *testing.T) {
	data := []byte("index v1")
	down := false
	var mu sync.Mutex
	files := map[string][]byte{"/index.txt": data}
	inner := fileServer(files)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		d := down
		mu.Unlock()
		if d {
			w.WriteHeader(500)
			return
		}
		inner(w, r)
	})
	r := newRig(t, proxyRaw(f, `, "ttl": "1m"`))
	r.expect(r.get("/repository/up/index.txt"), 200)
	r.expect(r.get("/repository/up/index.txt"), 200)
	if f.count() != 1 {
		t.Fatal("asked inside the time to live")
	}
	// After the time: asked with If-None-Match; unchanged is a 304 and the cache stays.
	r.advance(2 * time.Minute)
	r.expect(r.get("/repository/up/index.txt"), 200)
	if f.count() != 2 {
		t.Fatalf("not revalidated: %d", f.count())
	}
	// Down: the stale file is served, with a Warning, and it is counted.
	mu.Lock()
	down = true
	mu.Unlock()
	r.advance(2 * time.Minute)
	a := r.expect(r.get("/repository/up/index.txt"), 200)
	if !bytes.Equal(a.body, data) || a.hd.Get("Warning") == "" {
		t.Fatalf("stale: %q %v", a.body, a.hd)
	}
	if r.stale.Value("up") != 1 {
		t.Fatalf("stale count %d", r.stale.Value("up"))
	}
	// A file that was never cached is not served: 503.
	r.advance(time.Hour)
	if b := r.do("GET", "/repository/up/never.txt", nil); b.code != 503 {
		t.Fatalf("never cached while down: %d", b.code)
	}
	// And it comes back: a changed file replaces the cached one when the path is not immutable.
	mu.Lock()
	down = false
	files["/index.txt"] = []byte("index v2")
	mu.Unlock()
	r.advance(time.Hour)
	if b := r.expect(r.get("/repository/up/index.txt"), 200); string(b.body) != "index v2" {
		t.Fatalf("the changed file: %q", b.body)
	}
}

// P5, T18: other bytes for a file that cannot change: the cached bytes stay, the audit log and a metric say it.
func TestRawProxyChangedBytesAreAnAlarm(t *testing.T) {
	var mu sync.Mutex
	files := map[string][]byte{"/rel/1.0/lib.bin": blobBytes(5000, 7)}
	inner := fileServer(files)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { mu.Lock(); defer mu.Unlock(); inner(w, r) })
	r := newRig(t, proxyRaw(f, `, "ttl": "1m", "immutable_paths": ["rel/**"]`))
	orig := files["/rel/1.0/lib.bin"]
	r.expect(r.get("/repository/up/rel/1.0/lib.bin"), 200)
	mu.Lock()
	files["/rel/1.0/lib.bin"] = blobBytes(5000, 9) // replaced upstream
	mu.Unlock()
	r.advance(2 * time.Minute)
	a := r.expect(r.get("/repository/up/rel/1.0/lib.bin"), 200)
	if !bytes.Equal(a.body, orig) {
		t.Fatal("the cached bytes did not stay")
	}
	if r.changed.Value("up") != 1 || r.auditCount("upstream_changed", sum(blobBytes(5000, 9))) != 1 {
		t.Fatalf("the alarm: metric %d", r.changed.Value("up"))
	}
	// Not asked again at once: the check was made.
	n := f.count()
	r.expect(r.get("/repository/up/rel/1.0/lib.bin"), 200)
	if f.count() != n {
		t.Fatal("asked again inside the time")
	}
}

// P1: a digest that the upstream names and that the bytes do not have: the file is not kept, and the client's body is cut.
func TestRawProxyChecksTheDigestTheUpstreamNames(t *testing.T) {
	data := blobBytes(50000, 3)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Checksum-Sha256", sum([]byte("other")))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	})
	r := newRig(t, proxyRaw(f, ""))
	a := r.do("GET", "/repository/up/x.bin", nil)
	if a.err == nil && a.code == 200 && len(a.body) == len(data) {
		t.Fatal("a body that is not the named bytes was given whole")
	}
	rows, _ := r.db.Query(context.Background(), meta.S(`SELECT 1 FROM assets WHERE repo = 'up'`))
	if len(rows) != 0 {
		t.Fatal("kept")
	}
	if r.changed.Value("up") != 1 {
		t.Fatal("not counted")
	}
}

// P9: an address that is private is refused after the name is resolved, unless the upstream says allow_private.
func TestRawProxyRefusesPrivateAddresses(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("secret")) })
	set := repos(conf(`{"name": "up", "format": "raw", "kind": "proxy", "upstream": {"url": %q}}`, f.URL))
	r := newRig(t, set)
	a := r.do("GET", "/repository/up/inner", nil)
	if a.code != 502 || f.count() != 0 {
		t.Fatalf("a private upstream: %d, asked %d times: %s", a.code, f.count(), a.body)
	}
	// A redirect to a private address is refused as well.
	set = repos(conf(`{"name": "up", "format": "raw", "kind": "proxy", "upstream": {"url": %q}}`, "http://localhost:1"))
	r = newRig(t, set)
	if a := r.do("GET", "/repository/up/inner", nil); a.code != 502 {
		t.Fatalf("localhost: %d", a.code)
	}
}

// P8: credentials go to the upstream, and not to another host after a redirect.
func TestRawProxyCredentialsStayWithTheUpstream(t *testing.T) {
	t.Setenv("TEST_UP_PASSWORD", "s3cret-pass")
	cdn := newFake(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("from the cdn")) })
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "http://localhost:"+cdn.URL[len("http://127.0.0.1:"):]+"/file", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("direct"))
	})
	r := newRig(t, proxyRaw(f, `, "username": "bot", "password_env": "TEST_UP_PASSWORD"`))
	r.expect(r.get("/repository/up/direct"), 200)
	if len(f.auth) == 0 || f.auth[0] != "Basic Ym90OnMzY3JldC1wYXNz" {
		t.Fatalf("the upstream saw %q", f.auth)
	}
	a := r.expect(r.get("/repository/up/moved"), 200)
	if string(a.body) != "from the cdn" {
		t.Fatalf("redirect: %q", a.body)
	}
	for _, h := range cdn.auth {
		if h != "" {
			t.Fatalf("the other host was sent credentials: %q", h)
		}
	}
	// The password is not in the settings that the API shows, and an unset variable is an error, not a request with no credentials.
	r2 := newRig(t, proxyRaw(f, `, "username": "bot", "password_env": "TEST_NOT_SET_ANYWHERE"`))
	if a := r2.do("GET", "/repository/up/direct", nil); a.code != 502 {
		t.Fatalf("a missing secret: %d", a.code)
	}
}

// R2, R3, T20: a group of a hosted repository and a proxy: the first member that has the name answers; a name that the hosted repository has is never asked of the upstream, even when the proxy
// comes first; a claimed prefix is a 404, not a fetch; a write goes to write_to.
func TestRawGroup(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/tool.txt":
			_, _ = w.Write([]byte("PUBLIC IMPOSTOR")) // a name of ours, published upstream by someone else
		case "/public/lib.txt", "/claimed/x.txt":
			_, _ = w.Write([]byte("public lib"))
		default:
			http.NotFound(w, r)
		}
	})
	set := repos(
		`{"name": "mine", "format": "raw", "kind": "hosted"}`,
		conf(`{"name": "up", "format": "raw", "kind": "proxy", "upstream": {"url": %q, "allow_private": true}}`, f.URL),
		`{"name": "all", "format": "raw", "kind": "group", "members": ["up", "mine"], "write_to": "mine", "claims": ["claimed/"]}`,
		`{"name": "ro", "format": "raw", "kind": "group", "members": ["mine"]}`)
	r := newRig(t, set)
	// Writing to the group goes to "mine".
	r.expect(r.do("PUT", "/repository/all/internal/tool.txt", bytes.NewReader([]byte("our tool"))), 201)
	rows, _ := r.db.Query(context.Background(), meta.S(`SELECT repo FROM assets WHERE path = 'internal/tool.txt'`))
	if len(rows) != 1 || meta.Str(rows[0][0]) != "mine" {
		t.Fatalf("the write went to %v", rows)
	}
	// The proxy is first in the group, yet the name that "mine" has is ours, and the upstream is not asked.
	a := r.expect(r.get("/repository/all/internal/tool.txt"), 200)
	if string(a.body) != "our tool" || f.count() != 0 {
		t.Fatalf("dependency confusion: %q, the upstream was asked %d times", a.body, f.count())
	}
	// Even after our file is deleted the name stays ours (a name that was published is claimed).
	r.expect(r.do("DELETE", "/repository/all/internal/tool.txt", nil), 204)
	r.expect(r.get("/repository/all/internal/tool.txt"), 404)
	if f.count() != 0 {
		t.Fatal("a name that was ours went to the upstream after its delete")
	}
	// A claimed prefix: a 404, not a fetch, though the upstream has it.
	r.expect(r.get("/repository/all/claimed/x.txt"), 404)
	if f.count() != 0 {
		t.Fatal("a claimed prefix went to the upstream")
	}
	// A name that is nobody's goes to the proxy.
	b := r.expect(r.get("/repository/all/public/lib.txt"), 200)
	if string(b.body) != "public lib" {
		t.Fatal("the public file")
	}
	r.expect(r.get("/repository/all/public/none.txt"), 404)
	// A group with no write_to is read-only; listing a group is the hosted members'.
	r.expect(r.do("PUT", "/repository/ro/x", bytes.NewReader([]byte("x"))), 405)
	r.expect(r.do("PUT", "/repository/all/docs/a.txt", bytes.NewReader([]byte("a"))), 201)
	l := r.expect(r.get("/repository/all/docs/"), 200)
	if !bytes.Contains(l.body, []byte("docs/a.txt")) {
		t.Fatalf("the listing of a group: %s", l.body)
	}
}

// An upstream that is down is not a reason to ask the next member: the answer of a later member would stand in for the one that cannot answer.
func TestRawGroupDoesNotSkipAMemberThatIsDown(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	g := newFake(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("from the second")) })
	set := repos(
		conf(`{"name": "first", "format": "raw", "kind": "proxy", "upstream": {"url": %q, "allow_private": true}}`, f.URL),
		conf(`{"name": "second", "format": "raw", "kind": "proxy", "upstream": {"url": %q, "allow_private": true}}`, g.URL),
		`{"name": "all", "format": "raw", "kind": "group", "members": ["first", "second"]}`)
	r := newRig(t, set)
	a := r.do("GET", "/repository/all/x", nil)
	if a.code != 503 || g.count() != 0 {
		t.Fatalf("the first is down: %d, the second was asked %d times", a.code, g.count())
	}
}

// Docker Hub and GitHub's registry answer 401 for a name that is not there. A refusal for one name is "not found" for that name, remembered like one, and does not pause the upstream for the others.
func TestProxyDeniedNameIsNotAnUpstreamFailure(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.txt":
			_, _ = w.Write([]byte("fine"))
		case "/forbidden.txt":
			w.WriteHeader(403)
		default:
			w.WriteHeader(401)
		}
	})
	r := newRig(t, proxyRaw(f, ""))
	r.expect(r.get("/repository/up/secret.txt"), 404)
	r.expect(r.get("/repository/up/forbidden.txt"), 404)
	// No pause: the next name is asked at once, and what was denied is not asked again.
	r.expect(r.get("/repository/up/ok.txt"), 200)
	n := f.count()
	r.expect(r.get("/repository/up/secret.txt"), 404)
	if f.count() != n {
		t.Fatal("a denied name was asked again")
	}
	if r.errs.Value("up") != 0 {
		t.Fatalf("a refusal for a name was counted as an error of the upstream: %d", r.errs.Value("up"))
	}
}

// Credentials that every request carries and that are refused are the same for every name: that is an error of the use of the upstream, and it pauses.
func TestProxyRefusedCredentialsPause(t *testing.T) {
	t.Setenv("TEST_BAD_PASSWORD", "wrong")
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	r := newRig(t, proxyRaw(f, `, "username": "bot", "password_env": "TEST_BAD_PASSWORD"`))
	if a := r.do("GET", "/repository/up/x", nil); a.code != 502 {
		t.Fatalf("refused credentials: %d", a.code)
	}
	n := f.count()
	r.do("GET", "/repository/up/y", nil)
	if f.count() != n {
		t.Fatal("the upstream was asked during the pause")
	}
}
