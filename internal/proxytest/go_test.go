package proxytest

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// goZip makes a module zip, as the go command makes it.
func goZip(module, version string, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		f, _ := zw.Create(module + "@" + version + "/" + name)
		_, _ = f.Write([]byte(body))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func goMod(module string) map[string]string {
	return map[string]string{"go.mod": "module " + module + "\n\ngo 1.21\n", "x.go": "package x\n"}
}

const goSettings = `{"repositories": [
 {"name": "mods", "format": "go", "kind": "hosted"},
 {"name": "pub", "format": "go", "kind": "proxy", "upstream": {"url": %q, "allow_private": true}},
 {"name": "all", "format": "go", "kind": "group", "members": ["pub", "mods"], "write_to": "mods", "claims": ["corp.example.com/"]}
], "grants": []}`

// A false module proxy.
type goUp struct {
	mu   sync.Mutex
	list map[string][]string // module -> versions
	down bool
}

func (g *goUp) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.down {
			w.WriteHeader(500)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		for mod, vs := range g.list {
			switch {
			case p == mod+"/@v/list":
				_, _ = w.Write([]byte(strings.Join(vs, "\n") + "\n"))
				return
			case p == mod+"/@latest":
				_, _ = w.Write([]byte(`{"Version":"` + vs[len(vs)-1] + `","Time":"2026-01-01T00:00:00Z"}`))
				return
			}
			for _, v := range vs {
				switch p {
				case mod + "/@v/" + v + ".info":
					_, _ = w.Write([]byte(`{"Version":"` + v + `","Time":"2026-01-01T00:00:00Z"}`))
					return
				case mod + "/@v/" + v + ".mod":
					_, _ = w.Write([]byte("module " + mod + "\n"))
					return
				case mod + "/@v/" + v + ".zip":
					_, _ = w.Write(goZip(mod, v, goMod(mod)))
					return
				}
			}
		}
		if strings.HasSuffix(p, "/@v/v1.5.0.info") {
			w.WriteHeader(http.StatusGone)
			return
		}
		http.NotFound(w, r)
	}
}

func TestGoHosted(t *testing.T) {
	r := newRig(t, repos(`{"name": "mods", "format": "go", "kind": "hosted"}`))
	zip1 := goZip("example.com/Hello", "v1.0.0", goMod("example.com/Hello"))
	base := "/repository/mods/example.com/!hello/@v/"
	// Publish: the .mod and the .info are made from the zip.
	r.expect(r.do("PUT", base+"v1.0.0.zip", bytes.NewReader(zip1)), 201)
	r.expect(r.do("PUT", base+"v1.0.0.zip", bytes.NewReader(zip1)), 200) // the same bytes again
	mod := r.expect(r.get(base+"v1.0.0.mod"), 200)
	if !strings.Contains(string(mod.body), "module example.com/Hello") {
		t.Fatalf("the .mod: %q", mod.body)
	}
	var info struct{ Version, Time string }
	if err := json.Unmarshal(r.expect(r.get(base+"v1.0.0.info"), 200).body, &info); err != nil || info.Version != "v1.0.0" || info.Time == "" {
		t.Fatalf("the .info: %+v %v", info, err)
	}
	if z := r.expect(r.get(base+"v1.0.0.zip"), 200); !bytes.Equal(z.body, zip1) || z.hd.Get("Content-Type") != "application/zip" {
		t.Fatal("the zip is not what was published")
	}
	// Other bytes under a version: refused.
	r.expect(r.do("PUT", base+"v1.0.0.zip", bytes.NewReader(goZip("example.com/Hello", "v1.0.0", map[string]string{"go.mod": "module example.com/Hello\n", "other.go": "package y"}))), 409)
	// More versions, the list in order, the latest is a release.
	for _, v := range []string{"v1.10.0", "v1.2.0", "v2.0.0-rc.1"} {
		mod := "example.com/Hello"
		if strings.HasPrefix(v, "v2") {
			continue // a v2 needs its own path
		}
		r.expect(r.do("PUT", base+v+".zip", bytes.NewReader(goZip(mod, v, goMod(mod)))), 201)
	}
	l := r.expect(r.get(base+"list"), 200)
	if string(l.body) != "v1.0.0\nv1.2.0\nv1.10.0\n" {
		t.Fatalf("the list: %q", l.body)
	}
	if err := json.Unmarshal(r.expect(r.get("/repository/mods/example.com/!hello/@latest"), 200).body, &info); err != nil || info.Version != "v1.10.0" {
		t.Fatalf("latest: %+v", info)
	}
	// A prerelease is the latest only when there is no release.
	pre := goZip("example.com/pre", "v0.1.0-beta", goMod("example.com/pre"))
	r.expect(r.do("PUT", "/repository/mods/example.com/pre/@v/v0.1.0-beta.zip", bytes.NewReader(pre)), 201)
	if err := json.Unmarshal(r.expect(r.get("/repository/mods/example.com/pre/@latest"), 200).body, &info); err != nil || info.Version != "v0.1.0-beta" {
		t.Fatalf("latest of prereleases: %+v", info)
	}
	// A module that is not there, and paths that are not the protocol's.
	r.expect(r.get("/repository/mods/example.com/none/@v/list"), 404)
	r.expect(r.get("/repository/mods/example.com/none/@latest"), 404)
	r.expect(r.get("/repository/mods/whatever"), 404)
	// Unpublish: gone from the list, the files go with it.
	r.expect(r.do("DELETE", base+"v1.2.0.zip", nil), 204)
	r.expect(r.get(base+"v1.2.0.mod"), 404)
	r.expect(r.do("DELETE", base+"v1.2.0.zip", nil), 404)
	if l := r.expect(r.get(base+"list"), 200); string(l.body) != "v1.0.0\nv1.10.0\n" {
		t.Fatalf("the list after unpublish: %q", l.body)
	}
}

func TestGoPublishRefusals(t *testing.T) {
	r := newRig(t, repos(`{"name": "mods", "format": "go", "kind": "hosted"}`))
	put := func(path string, body []byte, want int) {
		t.Helper()
		r.expect(r.do("PUT", "/repository/mods/"+path, bytes.NewReader(body)), want)
	}
	good := goZip("example.com/m", "v1.0.0", goMod("example.com/m"))
	put("example.com/m/@v/v1.0.0.zip", good, 201)
	put("example.com/m/@v/v1.0.1.zip", good, 400)                             // the names in the zip are for v1.0.0
	put("example.com/m/@v/v1.0.0.mod", []byte("module example.com/m\n"), 400) // only the zip is put
	put("example.com/m/@v/v1.0.0.info", []byte("{}"), 400)
	put("example.com/m/@v/list", []byte("v1.0.0"), 400)
	put("example.com/n/@v/v1.0.0.zip", good, 400) // the zip is of another module
	put("example.com/m/@v/v1.0.2.zip", []byte("not a zip"), 400)
	put("example.com/m/@v/v1.0.2.zip", goZip("example.com/m", "v1.0.2", map[string]string{"x.go": "package x"}), 400) // no go.mod
	put("example.com/m/@v/v1.0.2.zip", goZip("example.com/m", "v1.0.2", map[string]string{"go.mod": "module example.com/other\n"}), 400)
	put("example.com/m/@v/v2.0.0.zip", goZip("example.com/m", "v2.0.0", goMod("example.com/m")), 400) // v2 needs /v2
	put("example.com/m/v2/@v/v2.0.0.zip", goZip("example.com/m/v2", "v2.0.0", goMod("example.com/m/v2")), 201)
	put("example.com/m/@v/v1.0.3.zip", goZip("example.com/m", "v1.0.3", map[string]string{"go.mod": "module example.com/m\n", "../evil": "x"}), 400)
	put("example.com/m/@v/v1.0.3.zip", goZip("example.com/m", "v1.0.3", map[string]string{"go.mod": "module example.com/m\n", "/abs": "x"}), 400)
	// A file outside the directory of the module.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("example.com/m@v1.0.4/go.mod")
	_, _ = f.Write([]byte("module example.com/m\n"))
	f, _ = zw.Create("elsewhere/x.go")
	_, _ = f.Write([]byte("x"))
	_ = zw.Close()
	put("example.com/m/@v/v1.0.4.zip", buf.Bytes(), 400)
	// Nothing of the refused ones is left: the list has the two good versions.
	if l := r.expect(r.get("/repository/mods/example.com/m/@v/list"), 200); string(l.body) != "v1.0.0\n" {
		t.Fatalf("the list: %q", l.body)
	}
	// An unknown token is not let publish; an anonymous request cannot read.
	req, _ := http.NewRequest("PUT", r.ts.URL+"/repository/mods/example.com/m/@v/v1.0.9.zip", bytes.NewReader(good))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("anonymous publish: %v %v", resp, err)
	}
}

// A proxy of a module proxy: a version is fetched once and kept; the list and the latest have a time to live; when the upstream is down the list is served stale.
func TestGoProxy(t *testing.T) {
	up := &goUp{list: map[string][]string{"github.com/!some/!mod": {"v1.0.0", "v1.1.0"}}}
	f := newFake(t, up.handler())
	r := newRig(t, `{"repositories": [{"name": "pub", "format": "go", "kind": "proxy", "upstream": {"url": "`+f.URL+`", "allow_private": true, "ttl": "5m"}}], "grants": []}`)
	base := "/repository/pub/github.com/!some/!mod/@v/"
	l := r.expect(r.get(base+"list"), 200)
	if string(l.body) != "v1.0.0\nv1.1.0\n" {
		t.Fatalf("list: %q", l.body)
	}
	for i := 0; i < 3; i++ {
		r.expect(r.get(base+"v1.1.0.zip"), 200)
		r.expect(r.get(base+"v1.1.0.mod"), 200)
		r.expect(r.get(base+"v1.1.0.info"), 200)
	}
	if n := f.asked("GET", "/github.com/!some/!mod/@v/v1.1.0.zip"); n != 1 {
		t.Fatalf("the zip of a version was fetched %d times", n)
	}
	r.get(base + "list") // inside the time to live
	if n := f.asked("GET", "/github.com/!some/!mod/@v/list"); n != 1 {
		t.Fatalf("the list was asked %d times inside its time", n)
	}
	// After the time it is asked again, and a new version shows.
	up.mu.Lock()
	up.list["github.com/!some/!mod"] = append(up.list["github.com/!some/!mod"], "v1.2.0")
	up.mu.Unlock()
	r.advance(6 * time.Minute)
	if l := r.expect(r.get(base+"list"), 200); !strings.Contains(string(l.body), "v1.2.0") {
		t.Fatalf("a new version is not in the list: %q", l.body)
	}
	// A version is never asked again, however long ago it was fetched.
	r.expect(r.get(base+"v1.1.0.zip"), 200)
	if n := f.asked("GET", "/github.com/!some/!mod/@v/v1.1.0.zip"); n != 1 {
		t.Fatalf("after the time to live the zip of a version was fetched %d times", n)
	}
	// Down: the list is served from the cache, stale.
	up.mu.Lock()
	up.down = true
	up.mu.Unlock()
	r.advance(6 * time.Minute)
	a := r.expect(r.get(base+"list"), 200)
	if a.hd.Get("Warning") == "" || !strings.Contains(string(a.body), "v1.2.0") {
		t.Fatalf("stale list: %v %q", a.hd, a.body)
	}
	// What was never cached is a 503 while it is down; the paths of the protocol only; read-only.
	if b := r.get(base + "v9.9.9.zip"); b.code != 503 {
		t.Fatalf("down: %d", b.code)
	}
	r.expect(r.get("/repository/pub/anything/else"), 404)
	r.expect(r.do("PUT", base+"v1.0.0.zip", bytes.NewReader([]byte("x"))), 405)
	// A module the upstream lacks is not found, and remembered.
	up.mu.Lock()
	up.down = false
	up.mu.Unlock()
	r.advance(time.Minute)
	r.expect(r.get("/repository/pub/github.com/x/y/@v/v1.5.0.info"), 404) // the answer 410 of a module proxy for a version it has removed
	r.expect(r.get("/repository/pub/github.com/none/@latest"), 404)
	n := f.count()
	r.expect(r.get("/repository/pub/github.com/none/@latest"), 404)
	if f.count() != n {
		t.Fatal("a module that was not found was asked again")
	}
}

// A group of a hosted repository and a proxy: one GOPROXY for what is ours and what is public; our module is never taken from the public proxy, whatever its version there.
func TestGoGroup(t *testing.T) {
	up := &goUp{list: map[string][]string{
		"github.com/google/uuid":    {"v1.6.0"},
		"corp.example.com/internal": {"v9.9.9"}, // the same name published by someone else
		"corp.example.com/other":    {"v1.0.0"},
		"example.com/ours":          {"v7.0.0"},
	}}
	f := newFake(t, up.handler())
	r := newRig(t, conf(goSettings, f.URL))
	// Ours, pushed through the group.
	zipOurs := goZip("example.com/ours", "v1.0.0", goMod("example.com/ours"))
	r.expect(r.do("PUT", "/repository/all/example.com/ours/@v/v1.0.0.zip", bytes.NewReader(zipOurs)), 201)
	// The group answers for the module with the member that has it: ours, though the proxy comes first and has v7.0.0.
	if l := r.expect(r.get("/repository/all/example.com/ours/@v/list"), 200); string(l.body) != "v1.0.0\n" {
		t.Fatalf("versions are mixed from the members: %q", l.body)
	}
	if n := f.asked("GET", "/example.com/ours/@v/list"); n != 0 {
		t.Fatalf("the public proxy was asked for our module %d times", n)
	}
	// A claimed prefix is not found, and not asked.
	r.expect(r.get("/repository/all/corp.example.com/internal/@v/list"), 404)
	r.expect(r.get("/repository/all/corp.example.com/other/@latest"), 404)
	r.expect(r.get("/repository/all/corp.example.com/internal/@v/v9.9.9.zip"), 404)
	for _, s := range f.seen {
		if strings.Contains(s, "corp.example.com") {
			t.Fatalf("a claimed prefix was asked: %s", s)
		}
	}
	// A public module comes from the proxy.
	if z := r.expect(r.get("/repository/all/github.com/google/uuid/@v/v1.6.0.zip"), 200); len(z.body) == 0 {
		t.Fatal("the public module")
	}
	// Unpublished: the name stays ours.
	r.expect(r.do("DELETE", "/repository/all/example.com/ours/@v/v1.0.0.zip", nil), 204)
	r.expect(r.get("/repository/all/example.com/ours/@v/list"), 404)
	if n := f.asked("GET", "/example.com/ours/@v/list"); n != 0 {
		t.Fatalf("after the unpublish the public proxy was asked for our module %d times", n)
	}
	// A group with no write_to takes no publish.
	r2 := newRig(t, `{"repositories": [{"name": "m", "format": "go", "kind": "hosted"}, {"name": "g", "format": "go", "kind": "group", "members": ["m"]}], "grants": []}`)
	r2.expect(r2.do("PUT", "/repository/g/example.com/ours/@v/v1.0.0.zip", bytes.NewReader(zipOurs)), 405)
}
