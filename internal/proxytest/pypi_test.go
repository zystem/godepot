package proxytest

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func wheel(name, version, requires string, extra ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	di := strings.ReplaceAll(name, "-", "_") + "-" + version + ".dist-info/"
	md := "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\n"
	if requires != "" {
		md += "Requires-Python: " + requires + "\n"
	}
	f, _ := zw.Create(di + "METADATA")
	_, _ = f.Write([]byte(md + "\nA package.\n"))
	f, _ = zw.Create(di + "WHEEL")
	_, _ = f.Write([]byte("Wheel-Version: 1.0\n"))
	f, _ = zw.Create(strings.ReplaceAll(name, "-", "_") + "/__init__.py")
	_, _ = f.Write([]byte("x = 1\n"))
	for _, e := range extra {
		f, _ = zw.Create(e)
		_, _ = f.Write([]byte("x"))
	}
	_ = zw.Close()
	return buf.Bytes()
}

// form makes the form that twine sends.
func form(fields map[string]string, filename string, content []byte) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if filename != "" {
		fw, _ := mw.CreateFormFile("content", filename)
		_, _ = fw.Write(content)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func (r *rig) upload(path, name, version, filename string, content []byte, extra ...string) reply {
	r.t.Helper()
	f := map[string]string{":action": "file_upload", "protocol_version": "1", "name": name, "version": version, "sha256_digest": sum(content)}
	for i := 0; i+1 < len(extra); i += 2 {
		f[extra[i]] = extra[i+1]
	}
	body, ct := form(f, filename, content)
	return r.do("POST", path, body, "Content-Type", ct)
}

func TestPyPIHosted(t *testing.T) {
	r := newRig(t, repos(`{"name": "py", "format": "pypi", "kind": "hosted"}`))
	whl := wheel("Foo-Bar", "1.0", ">=3.8")
	sdist := blobBytes(3000, 5)
	r.expect(r.upload("/repository/py/legacy/", "Foo_Bar", "1.0", "Foo_Bar-1.0-py3-none-any.whl", whl), 201)
	r.expect(r.upload("/repository/py/legacy/", "foo.bar", "1.0", "foo-bar-1.0.tar.gz", sdist), 201)
	r.expect(r.upload("/repository/py/legacy/", "Foo_Bar", "1.0", "Foo_Bar-1.0-py3-none-any.whl", whl), 200) // the same bytes again

	// The index, in HTML, with the hash, the Python requirement and the metadata file (PEP 503, 658).
	a := r.expect(r.get("/repository/py/simple/foo-bar/"), 200)
	html := string(a.body)
	if !strings.Contains(a.hd.Get("Content-Type"), "html") || !strings.Contains(html, `href="../../files/foo-bar/Foo_Bar-1.0-py3-none-any.whl#sha256=`+sum(whl)+`"`) ||
		!strings.Contains(html, `data-requires-python="&gt;=3.8"`) || !strings.Contains(html, "data-core-metadata=") || !strings.Contains(html, `foo-bar-1.0.tar.gz#sha256=`+sum(sdist)) {
		t.Fatalf("the index:\n%s", html)
	}
	// In JSON (PEP 691), asked as pip asks.
	j := r.expect(r.get("/repository/py/simple/foo-bar/", "Accept", "application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html; q=0.1"), 200)
	var idx struct {
		Name  string
		Files []struct {
			Filename string
			URL      string
			Hashes   map[string]string
			Yanked   any
			Core     any `json:"core-metadata"`
		}
		Versions []string
	}
	if err := json.Unmarshal(j.body, &idx); err != nil || idx.Name != "foo-bar" || len(idx.Files) != 2 || idx.Files[0].Hashes["sha256"] == "" || idx.Versions[0] != "1.0" {
		t.Fatalf("the JSON index: %s %v", j.body, err)
	}
	// A name that is not normalised is sent to the normalised one.
	r.expect(r.get("/repository/py/simple/Foo_Bar/"), 200)
	// The files, and the metadata file of the wheel.
	if f := r.expect(r.get("/repository/py/files/foo-bar/Foo_Bar-1.0-py3-none-any.whl"), 200); !bytes.Equal(f.body, whl) {
		t.Fatal("the wheel")
	}
	if m := r.expect(r.get("/repository/py/files/foo-bar/Foo_Bar-1.0-py3-none-any.whl.metadata"), 200); !strings.Contains(string(m.body), "Name: Foo-Bar") {
		t.Fatalf("the metadata file: %q", m.body)
	}
	// Other bytes under a file name: refused.
	r.expect(r.upload("/repository/py/legacy/", "foo-bar", "1.0", "foo-bar-1.0.tar.gz", blobBytes(3000, 6)), 409)
	// The root index.
	if root := r.expect(r.get("/repository/py/simple/"), 200); !strings.Contains(string(root.body), `<a href="foo-bar/">foo-bar</a>`) {
		t.Fatalf("root: %s", root.body)
	}
	// Yank (PEP 592) and lift.
	r.expect(r.do("POST", "/repository/py/yank/foo-bar/foo-bar-1.0.tar.gz", strings.NewReader("broken build")), 204)
	if h := string(r.get("/repository/py/simple/foo-bar/").body); !strings.Contains(h, `data-yanked="broken build"`) {
		t.Fatalf("yanked: %s", h)
	}
	if err := json.Unmarshal(r.get("/repository/py/simple/foo-bar/", "Accept", mimeJSONForTest).body, &idx); err != nil {
		t.Fatal(err)
	}
	yanked := ""
	for _, f := range idx.Files {
		if s, ok := f.Yanked.(string); ok {
			yanked = s
		}
	}
	if yanked != "broken build" {
		t.Fatalf("the JSON yanked reason: %q", yanked)
	}
	r.expect(r.do("DELETE", "/repository/py/yank/foo-bar/foo-bar-1.0.tar.gz", nil), 204)
	if h := string(r.get("/repository/py/simple/foo-bar/").body); strings.Contains(h, "data-yanked") {
		t.Fatal("the mark was not lifted")
	}
	r.expect(r.do("POST", "/repository/py/yank/foo-bar/none-1.0.tar.gz", nil), 404)
	// Delete a file: the metadata goes with it.
	r.expect(r.do("DELETE", "/repository/py/files/foo-bar/Foo_Bar-1.0-py3-none-any.whl", nil), 204)
	r.expect(r.get("/repository/py/files/foo-bar/Foo_Bar-1.0-py3-none-any.whl.metadata"), 404)
	r.expect(r.get("/repository/py/simple/nothing/"), 404)
	r.expect(r.get("/repository/py/index/foo-bar.json"), 404) // what a proxy caches is not an address of a client
}

const mimeJSONForTest = "application/vnd.pypi.simple.v1+json"

func TestPyPIUploadRefusals(t *testing.T) {
	r := newRig(t, repos(`{"name": "py", "format": "pypi", "kind": "hosted"}`))
	whl := wheel("pkg", "1.0", "")
	up := func(name, version, filename string, content []byte, want int, extra ...string) {
		t.Helper()
		r.expect(r.upload("/repository/py/legacy/", name, version, filename, content, extra...), want)
	}
	up("pkg", "1.0", "pkg-1.0-py3-none-any.whl", whl, 201)
	up("other", "1.0", "pkg-1.1-py3-none-any.whl", whl, 400)                                // the name is not the file's
	up("pkg", "2.0", "pkg-1.1-py3-none-any.whl", whl, 400)                                  // the version is not the file's
	up("pkg", "1.1", "pkg-1.1-py3-none-any.whl", whl, 400)                                  // METADATA says 1.0
	up("pkg", "1.2", "pkg-1.2.exe", whl, 400)                                               // not a distribution
	up("pkg", "1.2", "pkg-1.2.tar.gz", []byte("x"), 400, "sha256_digest", sum([]byte("y"))) // the digest is not the bytes
	up("pkg", "1.3", "pkg-1.3-py3-none-any.whl", []byte("not a zip"), 400)
	up("pkg", "1.4", "pkg-1.4-py3-none-any.whl", wheel("pkg", "1.4", "", "../evil"), 400)
	up("pkg", "1.5", "pkg-1.5-py3-none-any.whl", wheel("pkg", "1.5", "", "/abs"), 400)
	up("pkg", "1.6", "pkg-1.6-py3-none-any.whl", wheel("other", "1.6", ""), 400) // the METADATA is of another project
	r.expect(r.do("POST", "/repository/py/legacy/", strings.NewReader("x"), "Content-Type", "text/plain"), 400)
	body, ct := form(map[string]string{"name": "pkg", "version": "1.0"}, "", nil)
	r.expect(r.do("POST", "/repository/py/legacy/", body, "Content-Type", ct), 400) // no file
	body, ct = form(map[string]string{":action": "doc_upload", "name": "pkg", "version": "1.7"}, "pkg-1.7.tar.gz", []byte("x"))
	r.expect(r.do("POST", "/repository/py/legacy/", body, "Content-Type", ct), 400)
	// Only the good one is there.
	if h := string(r.get("/repository/py/simple/pkg/").body); strings.Count(h, "<a ") != 1 {
		t.Fatalf("the index after the refusals:\n%s", h)
	}
	// An anonymous upload is refused.
	req, _ := http.NewRequest("POST", r.ts.URL+"/repository/py/legacy/", strings.NewReader("x"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("anonymous upload: %v %v", resp, err)
	}
}

// A false PyPI: an index in JSON that names files on another host, as pypi.org does.
type pyUp struct {
	mu     sync.Mutex
	files  map[string][]byte // filename -> bytes
	down   bool
	hashes map[string]string // filename -> the hash the index names (default: the real one)
	meta   map[string][]byte // filename -> its metadata
	fhost  *fake
}

func (p *pyUp) indexHandler(project string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.down {
			w.WriteHeader(500)
			return
		}
		if r.URL.Path != "/simple/"+project+"/" {
			http.NotFound(w, r)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), mimeJSONForTest) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>not json</html>"))
			return
		}
		var files []map[string]any
		for fn, b := range p.files {
			h := sum(b)
			if o, ok := p.hashes[fn]; ok {
				h = o
			}
			e := map[string]any{"filename": fn, "url": p.fhost.URL + "/packages/" + fn, "hashes": map[string]string{"sha256": h}, "requires-python": ">=3.7", "yanked": false}
			if m, ok := p.meta[fn]; ok {
				e["core-metadata"] = map[string]string{"sha256": sum(m)}
			}
			files = append(files, e)
		}
		w.Header().Set("Content-Type", mimeJSONForTest)
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"api-version": "1.1"}, "name": project, "files": files})
	}
}

func (p *pyUp) fileHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		fn := strings.TrimPrefix(r.URL.Path, "/packages/")
		if m, ok := strings.CutSuffix(fn, ".metadata"); ok {
			if b, ok := p.meta[m]; ok {
				_, _ = w.Write(b)
				return
			}
		}
		if b, ok := p.files[fn]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}
}

func newPyUp(t *testing.T, project string, files map[string][]byte) (*pyUp, *fake, *fake) {
	up := &pyUp{files: files, hashes: map[string]string{}, meta: map[string][]byte{}}
	up.fhost = newFake(t, up.fileHandler())
	idx := newFake(t, up.indexHandler(project))
	return up, idx, up.fhost
}

func TestPyPIProxy(t *testing.T) {
	whl := wheel("six", "1.16.0", ">=2.7")
	up, idx, files := newPyUp(t, "six", map[string][]byte{"six-1.16.0-py2.py3-none-any.whl": whl, "six-1.15.0.tar.gz": blobBytes(2000, 3)})
	up.meta["six-1.16.0-py2.py3-none-any.whl"] = []byte("Metadata-Version: 2.1\nName: six\nVersion: 1.16.0\n")
	set := `{"repositories": [{"name": "pub", "format": "pypi", "kind": "proxy", "upstream": {"url": "` + idx.URL + `", "allow_private": true, "file_hosts": ["127.0.0.1"], "ttl": "5m"}}], "grants": []}`
	r := newRig(t, set)
	h := string(r.expect(r.get("/repository/pub/simple/six/"), 200).body)
	if !strings.Contains(h, "../../files/six/six-1.16.0-py2.py3-none-any.whl#sha256="+sum(whl)) || !strings.Contains(h, `data-requires-python="&gt;=3.7"`) || !strings.Contains(h, "data-core-metadata=\"sha256="+sum(up.meta["six-1.16.0-py2.py3-none-any.whl"])) {
		t.Fatalf("the index of the proxy:\n%s", h)
	}
	// The file is at the host the index names; it is fetched once, with its hash checked, and kept.
	for i := 0; i < 3; i++ {
		if f := r.expect(r.get("/repository/pub/files/six/six-1.16.0-py2.py3-none-any.whl"), 200); !bytes.Equal(f.body, whl) {
			t.Fatal("the file")
		}
	}
	if files.count() != 1 {
		t.Fatalf("the file host was asked %d times", files.count())
	}
	if m := r.expect(r.get("/repository/pub/files/six/six-1.16.0-py2.py3-none-any.whl.metadata"), 200); !strings.Contains(string(m.body), "Name: six") {
		t.Fatalf("metadata: %q", m.body)
	}
	// The index has a time to live; inside it nothing goes upstream, and after it the index is asked again.
	r.get("/repository/pub/simple/six/")
	if idx.count() != 1 {
		t.Fatalf("the index was asked %d times inside its time", idx.count())
	}
	r.advance(6 * time.Minute)
	r.expect(r.get("/repository/pub/simple/six/"), 200)
	if idx.count() != 2 {
		t.Fatalf("the index was not asked again after its time: %d", idx.count())
	}
	// When the upstream is down the index is served stale.
	up.mu.Lock()
	up.down = true
	up.mu.Unlock()
	r.advance(6 * time.Minute)
	a := r.expect(r.get("/repository/pub/simple/six/"), 200)
	if a.hd.Get("Warning") == "" {
		t.Fatalf("stale: %v", a.hd)
	}
	// Names are normalised; a project the upstream lacks is not found; read-only; a project that was never fetched is a 503 while the upstream is down.
	r.expect(r.get("/repository/pub/simple/Six/"), 200)
	r.expect(r.do("POST", "/repository/pub/legacy/", strings.NewReader("x")), 405)
	r.expect(r.get("/repository/pub/simple/"), 404)
	up.mu.Lock()
	up.down = false
	up.mu.Unlock()
	r.advance(time.Minute)
	r.expect(r.get("/repository/pub/simple/nothing/"), 404)
	// A file that the index does not list is not found.
	r.expect(r.get("/repository/pub/files/six/six-0.1.tar.gz"), 404)
}

// P1, P5: the hash that the index names is checked; and a file on a host that is not the upstream's and not in file_hosts is not fetched.
func TestPyPIProxyChecksHashesAndHosts(t *testing.T) {
	good := blobBytes(5000, 8)
	up, idx, files := newPyUp(t, "pkg", map[string][]byte{"pkg-1.0.tar.gz": good, "pkg-2.0.tar.gz": blobBytes(5000, 9)})
	up.hashes["pkg-2.0.tar.gz"] = sum([]byte("something else"))
	set := func(hosts string) string {
		return `{"repositories": [{"name": "pub", "format": "pypi", "kind": "proxy", "upstream": {"url": "` + idx.URL + `", "allow_private": true` + hosts + `}}], "grants": []}`
	}
	r := newRig(t, set(`, "file_hosts": ["127.0.0.1"]`))
	r.expect(r.get("/repository/pub/files/pkg/pkg-1.0.tar.gz"), 200)
	a := r.do("GET", "/repository/pub/files/pkg/pkg-2.0.tar.gz", nil)
	if a.err == nil && a.code == 200 && len(a.body) == 5000 {
		t.Fatal("a file whose bytes are not the hash of the index was given whole")
	}
	if r.changed.Value("pub") != 1 {
		t.Fatalf("not counted: %d", r.changed.Value("pub"))
	}
	// The same index, with no file_hosts: the file host is another host (a different port counts as the same host name, so a name that differs is used).
	r2 := newRig(t, set(``))
	if f := r2.do("GET", "/repository/pub/files/pkg/pkg-1.0.tar.gz", nil); f.code == 200 {
		t.Fatalf("a file on a host that is not the upstream's was fetched (%d bytes)", len(f.body))
	}
	_ = files
}

const pySettings = `{"repositories": [
 {"name": "mine", "format": "pypi", "kind": "hosted"},
 {"name": "pub", "format": "pypi", "kind": "proxy", "upstream": {"url": %q, "allow_private": true, "file_hosts": ["127.0.0.1"]}},
 {"name": "all", "format": "pypi", "kind": "group", "members": ["pub", "mine"], "write_to": "mine", "claims": ["corp-"]}
], "grants": []}`

// A group: one index URL for ours and the public; our project is never asked of the public index, even if the same name is there with a higher version; a claimed prefix is not fetched.
func TestPyPIGroup(t *testing.T) {
	up, idx, _ := newPyUp(t, "requests", map[string][]byte{"requests-9.0.tar.gz": blobBytes(1000, 1)})
	r := newRig(t, conf(pySettings, idx.URL))
	whl := wheel("requests", "1.0", "")
	// Uploaded through the group: it goes to "mine" (and "requests" is now ours, though the public index has it).
	r.expect(r.upload("/repository/all/legacy/", "requests", "1.0", "requests-1.0-py3-none-any.whl", whl), 201)
	h := string(r.expect(r.get("/repository/all/simple/requests/"), 200).body)
	if !strings.Contains(h, "requests-1.0-py3-none-any.whl") || strings.Contains(h, "requests-9.0") {
		t.Fatalf("versions are mixed from the members, or ours is not served:\n%s", h)
	}
	if idx.count() != 0 {
		t.Fatalf("the public index was asked for our project: %v", idx.seen)
	}
	if f := r.expect(r.get("/repository/all/files/requests/requests-1.0-py3-none-any.whl"), 200); !bytes.Equal(f.body, whl) {
		t.Fatal("our file through the group")
	}
	// A claimed prefix is not found and not asked.
	r.expect(r.get("/repository/all/simple/corp-internal/"), 404)
	if idx.count() != 0 {
		t.Fatal("a claimed prefix was asked")
	}
	// Yank through the group goes to write_to.
	r.expect(r.do("POST", "/repository/all/yank/requests/requests-1.0-py3-none-any.whl", strings.NewReader("no")), 204)
	// Deleted, the name stays ours.
	r.expect(r.do("DELETE", "/repository/all/files/requests/requests-1.0-py3-none-any.whl", nil), 204)
	r.expect(r.get("/repository/all/simple/requests/"), 404)
	r.expect(r.get("/repository/all/simple/Requests/"), 404) // the same name, not normalised
	if idx.count() != 0 {
		t.Fatal("after the delete the public index was asked for our name")
	}
	// Another project comes from the public index, through the group, and its file too.
	up.mu.Lock()
	up.files["six-1.0.tar.gz"] = blobBytes(500, 2)
	up.mu.Unlock()
	up2, idx2, _ := newPyUp(t, "six", map[string][]byte{"six-1.0.tar.gz": blobBytes(500, 2)})
	_ = up2
	r2 := newRig(t, conf(pySettings, idx2.URL))
	g := string(r2.expect(r2.get("/repository/all/simple/six/"), 200).body)
	if !strings.Contains(g, "../../files/six/six-1.0.tar.gz#sha256=") {
		t.Fatalf("a public project through the group:\n%s", g)
	}
	if f := r2.expect(r2.get("/repository/all/files/six/six-1.0.tar.gz"), 200); !bytes.Equal(f.body, blobBytes(500, 2)) {
		t.Fatal("a public file through the group")
	}
	// A group with no write_to takes no upload.
	r3 := newRig(t, `{"repositories": [{"name": "m", "format": "pypi", "kind": "hosted"}, {"name": "g", "format": "pypi", "kind": "group", "members": ["m"]}], "grants": []}`)
	r3.expect(r3.upload("/repository/g/legacy/", "x", "1", "x-1.tar.gz", []byte("x")), 405)
	_ = fmt.Sprint
}
