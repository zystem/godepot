package proxytest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func upSettings(format, url, extra string) string {
	return `{"repositories": [{"name": "pub", "format": "` + format + `", "kind": "proxy", "upstream": {"url": "` + url + `", "allow_private": true, "ttl": "5m"` + extra + `}}], "grants": []}`
}

// addAt puts a version in the false npm registry with the time it was published (the full packument has `time`).
func (u *npmUp) addAt(name, version string, data []byte, at time.Time) {
	u.add(name, version, data, true)
	u.mu.Lock()
	defer u.mu.Unlock()
	d := u.docs[name]
	if d["time"] == nil {
		d["time"] = map[string]string{}
	}
	d["time"].(map[string]string)[version] = at.UTC().Format("2006-01-02T15:04:05.000Z")
}

func TestNPMCooldownAndLists(t *testing.T) {
	up := newNPMUp(t)
	old, young := tgz("left-pad", "1.0.0", nil), tgz("left-pad", "1.1.0", nil)
	now := time.Now()
	up.addAt("left-pad", "1.0.0", old, now.Add(-30*24*time.Hour))
	up.addAt("left-pad", "1.1.0", young, now.Add(-time.Hour)) // the upstream's latest
	up.addAt("@types/node", "20.0.0", tgz("@types/node", "20.0.0", nil), now.Add(-time.Minute))
	r := newRig(t, upSettings("npm", up.URL, `, "cooldown": "168h", "allow": ["@types/*"]`))

	var d struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]any
	}
	a := r.expect(r.get("/repository/pub/left-pad", "Accept", abbrevAccept), 200)
	if err := json.Unmarshal(a.body, &d); err != nil || len(d.Versions) != 1 || d.Versions["1.0.0"] == nil || d.DistTags["latest"] != "1.0.0" {
		t.Fatalf("the young version should be out of the document, and latest should move to what is left: %s", a.body)
	}
	// By name: 403 with the words, and the time it will be served.
	f := r.expect(r.get("/repository/pub/left-pad/-/left-pad-1.1.0.tgz"), 403)
	if !strings.Contains(string(f.body), "left-pad@1.1.0") || !strings.Contains(string(f.body), "will be served from") || f.hd.Get("Retry-After") == "" {
		t.Fatalf("the refusal: %s %v", f.body, f.hd)
	}
	if up.asked("GET", "/left-pad/-/left-pad-1.1.0.tgz") != 0 {
		t.Fatal("the held-back tarball was fetched from the upstream")
	}
	if g := r.expect(r.get("/repository/pub/left-pad/-/left-pad-1.0.0.tgz"), 200); !bytes.Equal(g.body, old) {
		t.Fatal("the old tarball")
	}
	// The tags answer is filtered too.
	tags := map[string]string{}
	_ = json.Unmarshal(r.expect(r.get("/repository/pub/-/package/left-pad/dist-tags"), 200).body, &tags)
	if tags["latest"] != "1.0.0" {
		t.Fatalf("tags: %v", tags)
	}
	// The allow list lifts the cooldown for a name.
	r.expect(r.get("/repository/pub/@types%2fnode/-/node-20.0.0.tgz"), 200)
	// After the cooldown the version is there: the clock moves, the packument is asked again.
	r.advance(8 * 24 * time.Hour)
	a = r.expect(r.get("/repository/pub/left-pad", "Accept", abbrevAccept), 200)
	_ = json.Unmarshal(a.body, &d)
	if len(d.Versions) != 2 || d.DistTags["latest"] != "1.1.0" {
		t.Fatalf("after the cooldown: %s", a.body)
	}
	r.expect(r.get("/repository/pub/left-pad/-/left-pad-1.1.0.tgz"), 200)
}

func TestNPMBlockList(t *testing.T) {
	up := newNPMUp(t)
	good, bad := tgz("pkg", "1.0.0", nil), tgz("pkg", "1.1.0", map[string]string{"package/x.js": "evil"})
	up.addAt("pkg", "1.0.0", good, time.Now().Add(-48*time.Hour))
	up.addAt("pkg", "1.1.0", bad, time.Now().Add(-48*time.Hour))
	up.addAt("other", "1.0.0", tgz("other", "1.0.0", nil), time.Now().Add(-48*time.Hour))
	up.addAt("thing", "2.0.0", tgz("thing", "2.0.0", nil), time.Now().Add(-48*time.Hour))
	// A digest of the tarball of thing is blocked: the packument is fine, the bytes are refused.
	thing := up.tars["thing/-/thing-2.0.0.tgz"]
	r := newRig(t, upSettings("npm", up.URL, `, "block": ["pkg@1.1.*", "other", "sha256:`+sum(thing)+`"]`))
	var d struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]any
	}
	_ = json.Unmarshal(r.expect(r.get("/repository/pub/pkg", "Accept", abbrevAccept), 200).body, &d)
	if len(d.Versions) != 1 || d.DistTags["latest"] != "1.0.0" {
		t.Fatalf("pkg: %+v", d)
	}
	b := r.expect(r.get("/repository/pub/pkg/-/pkg-1.1.0.tgz"), 403)
	if !strings.Contains(string(b.body), "block list") {
		t.Fatalf("%s", b.body)
	}
	r.expect(r.get("/repository/pub/pkg/-/pkg-1.0.0.tgz"), 200)
	_ = json.Unmarshal(r.expect(r.get("/repository/pub/other", "Accept", abbrevAccept), 404).body, &d) // every version is blocked: no package
	r.expect(r.get("/repository/pub/thing", "Accept", abbrevAccept), 200)
	req, _ := http.NewRequest("GET", r.ts.URL+"/repository/pub/thing/-/thing-2.0.0.tgz", nil)
	req.Header.Set("Authorization", "Bearer "+r.admin)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		got, rerr := readAll(resp)
		if rerr == nil && resp.StatusCode == 200 && bytes.Equal(got, thing) {
			t.Fatal("bytes on the block list by digest were let through whole")
		}
	}
	// Nothing was kept: a second ask is cut again, never served.
	if code, body := r.tryGet("/repository/pub/thing/-/thing-2.0.0.tgz"); code == 200 && bytes.Equal(body, thing) {
		t.Fatal("the blocked bytes were kept and served")
	}
}

// A false PyPI upstream whose JSON index has the time of each upload.
type pyTimeUp struct {
	*fake
	mu    sync.Mutex
	files map[string][]byte
	at    map[string]time.Time
}

func newPyTimeUp(t *testing.T, project string, files map[string][]byte, at map[string]time.Time) *pyTimeUp {
	u := &pyTimeUp{files: files, at: at}
	u.fake = newFake(t, func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if r.URL.Path == "/simple/"+project+"/" {
			var fs []map[string]any
			for name, b := range u.files {
				fs = append(fs, map[string]any{"filename": name, "url": "/files/" + name, "hashes": map[string]string{"sha256": sum(b)}, "size": len(b), "upload-time": u.at[name].UTC().Format(time.RFC3339)})
			}
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"api-version": "1.0"}, "name": project, "files": fs})
			return
		}
		if name, ok := strings.CutPrefix(r.URL.Path, "/files/"); ok {
			if b, ok := u.files[name]; ok {
				_, _ = w.Write(b)
				return
			}
		}
		http.NotFound(w, r)
	})
	return u
}

func TestPyPICooldownAndLists(t *testing.T) {
	now := time.Now()
	oldF, newF := blobBytes(1500, 1), blobBytes(1500, 2)
	up := newPyTimeUp(t, "demo", map[string][]byte{"demo-1.0.tar.gz": oldF, "demo-1.1.tar.gz": newF, "demo-0.9.tar.gz": blobBytes(900, 3)},
		map[string]time.Time{"demo-1.0.tar.gz": now.Add(-40 * 24 * time.Hour), "demo-1.1.tar.gz": now.Add(-2 * time.Hour), "demo-0.9.tar.gz": now.Add(-90 * 24 * time.Hour)})
	r := newRig(t, upSettings("pypi", up.URL, `, "cooldown": "72h", "block": ["demo@0.9", "sha256:`+sum([]byte("nothing"))+`"]`))
	h := string(r.expect(r.get("/repository/pub/simple/demo/"), 200).body)
	if !strings.Contains(h, "demo-1.0.tar.gz") || strings.Contains(h, "demo-1.1.tar.gz") || strings.Contains(h, "demo-0.9.tar.gz") {
		t.Fatalf("the index should have only 1.0:\n%s", h)
	}
	young := r.expect(r.get("/repository/pub/files/demo/demo-1.1.tar.gz"), 403)
	if !strings.Contains(string(young.body), "will be served from") {
		t.Fatalf("%s", young.body)
	}
	blocked := r.expect(r.get("/repository/pub/files/demo/demo-0.9.tar.gz"), 403)
	if !strings.Contains(string(blocked.body), "block list") {
		t.Fatalf("%s", blocked.body)
	}
	if f := r.expect(r.get("/repository/pub/files/demo/demo-1.0.tar.gz"), 200); !bytes.Equal(f.body, oldF) {
		t.Fatal("the old file")
	}
	// The time passes; the index is asked again after its ttl.
	r.advance(4 * 24 * time.Hour)
	h = string(r.expect(r.get("/repository/pub/simple/demo/"), 200).body)
	if !strings.Contains(h, "demo-1.1.tar.gz") || strings.Contains(h, "demo-0.9.tar.gz") {
		t.Fatalf("after the cooldown:\n%s", h)
	}
	r.expect(r.get("/repository/pub/files/demo/demo-1.1.tar.gz"), 200)
	// A digest on the block list hides the file from the index and refuses it.
	r2 := newRig(t, upSettings("pypi", up.URL, `, "block": ["sha256:`+sum(oldF)+`"]`))
	if h := string(r2.expect(r2.get("/repository/pub/simple/demo/"), 200).body); strings.Contains(h, "demo-1.0.tar.gz") || !strings.Contains(h, "demo-1.1.tar.gz") {
		t.Fatalf("by digest:\n%s", h)
	}
	r2.expect(r2.get("/repository/pub/files/demo/demo-1.0.tar.gz"), 403)
}

type goTimeUp struct {
	*fake
	times map[string]time.Time // version -> published
}

func newGoTimeUp(t *testing.T, module string, times map[string]time.Time) *goTimeUp {
	u := &goTimeUp{times: times}
	u.fake = newFake(t, func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		var vs []string
		for v := range times {
			vs = append(vs, v)
		}
		sortVersions(vs)
		switch {
		case p == module+"/@v/list":
			_, _ = w.Write([]byte(strings.Join(vs, "\n") + "\n"))
		case p == module+"/@latest":
			v := vs[len(vs)-1]
			_, _ = w.Write([]byte(`{"Version":"` + v + `","Time":"` + times[v].UTC().Format(time.RFC3339) + `"}`))
		default:
			for _, v := range vs {
				switch p {
				case module + "/@v/" + v + ".info":
					_, _ = w.Write([]byte(`{"Version":"` + v + `","Time":"` + times[v].UTC().Format(time.RFC3339) + `"}`))
					return
				case module + "/@v/" + v + ".mod":
					_, _ = w.Write([]byte("module " + module + "\n"))
					return
				case module + "/@v/" + v + ".zip":
					_, _ = w.Write(goZip(module, v, goMod(module)))
					return
				}
			}
			http.NotFound(w, r)
		}
	})
	return u
}

func sortVersions(vs []string) {
	for i := range vs {
		for j := i + 1; j < len(vs); j++ {
			if vs[j] < vs[i] {
				vs[i], vs[j] = vs[j], vs[i]
			}
		}
	}
}

func TestGoCooldownAndLists(t *testing.T) {
	now := time.Now()
	mod := "example.com/lib"
	up := newGoTimeUp(t, mod, map[string]time.Time{"v1.0.0": now.Add(-90 * 24 * time.Hour), "v1.1.0": now.Add(-60 * 24 * time.Hour), "v1.2.0": now.Add(-3 * time.Hour), "v1.3.0": now.Add(-2 * time.Hour)})
	r := newRig(t, upSettings("go", up.URL, `, "cooldown": "24h", "block": ["example.com/lib@v1.1.0"]`))
	list := string(r.expect(r.get("/repository/pub/"+mod+"/@v/list"), 200).body)
	if strings.TrimSpace(list) != "v1.0.0" {
		t.Fatalf("the list should be v1.0.0 only (v1.1.0 is blocked, the others are young): %q", list)
	}
	var in struct{ Version string }
	_ = json.Unmarshal(r.expect(r.get("/repository/pub/"+mod+"/@latest"), 200).body, &in)
	if in.Version != "v1.0.0" {
		t.Fatalf("latest: %+v", in)
	}
	r.expect(r.get("/repository/pub/"+mod+"/@v/v1.0.0.zip"), 200)
	if y := r.expect(r.get("/repository/pub/"+mod+"/@v/v1.2.0.mod"), 403); !strings.Contains(string(y.body), "will be served from") {
		t.Fatalf("%s", y.body)
	}
	if b := r.expect(r.get("/repository/pub/"+mod+"/@v/v1.1.0.info"), 403); !strings.Contains(string(b.body), "block list") {
		t.Fatalf("%s", b.body)
	}
	if up.asked("GET", "/"+mod+"/@v/v1.2.0.mod") != 0 {
		t.Fatal("a held-back .mod was fetched")
	}
	r.advance(2 * 24 * time.Hour)
	list = string(r.expect(r.get("/repository/pub/"+mod+"/@v/list"), 200).body)
	if strings.Fields(list)[len(strings.Fields(list))-1] != "v1.3.0" || strings.Contains(list, "v1.1.0") || len(strings.Fields(list)) != 3 {
		t.Fatalf("after the cooldown: %q", list)
	}
}

func TestMavenAndRawBlockLists(t *testing.T) {
	up := newMavenUp(t)
	up.add("org/lib/core/1.0/core-1.0.jar", []byte("one"), "sha1")
	up.add("org/lib/core/2.0/core-2.0.jar", []byte("two"), "sha1")
	up.add("org/lib/core/maven-metadata.xml", []byte("<metadata>\n  <groupId>org.lib</groupId>\n  <artifactId>core</artifactId>\n  <versioning>\n    <latest>2.0</latest>\n    <release>2.0</release>\n    <versions>\n      <version>1.0</version>\n      <version>2.0</version>\n    </versions>\n  </versioning>\n</metadata>\n"), "sha1")
	r := newRig(t, upSettings("maven", up.URL, `, "block": ["org.lib:core@2.*"]`))
	md := string(r.expect(r.get("/repository/pub/org/lib/core/maven-metadata.xml"), 200).body)
	if strings.Contains(md, "2.0") || !strings.Contains(md, "<latest>1.0</latest>") || !strings.Contains(md, "<release>1.0</release>") || !strings.Contains(md, "<version>1.0</version>") {
		t.Fatalf("the metadata without the blocked version:\n%s", md)
	}
	if g := r.expect(r.get("/repository/pub/org/lib/core/maven-metadata.xml.sha1"), 200); string(g.body) != sha1hex([]byte(md)) {
		t.Fatalf("the checksum is of the metadata that is served: %s", g.body)
	}
	r.expect(r.get("/repository/pub/org/lib/core/2.0/core-2.0.jar"), 403)
	r.expect(r.get("/repository/pub/org/lib/core/2.0/core-2.0.jar.sha1"), 403)
	r.expect(r.get("/repository/pub/org/lib/core/1.0/core-1.0.jar"), 200)

	// The format raw: a path pattern, and a digest.
	rawUp := newFake(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("body of " + r.URL.Path)) })
	r2 := newRig(t, upSettings("raw", rawUp.URL, `, "block": ["secret/**", "sha256:`+sum([]byte("body of /by-digest.bin"))+`"]`))
	r2.expect(r2.get("/repository/pub/ok/a.bin"), 200)
	r2.expect(r2.get("/repository/pub/secret/a.bin"), 403)
	if rawUp.asked("GET", "/secret/a.bin") != 0 {
		t.Fatal("a blocked path was fetched")
	}
	for i := 0; i < 2; i++ { // the first ask fetches and cuts; the second finds nothing was kept
		if code, body := r2.tryGet("/repository/pub/by-digest.bin"); code == 200 && string(body) == "body of /by-digest.bin" {
			t.Fatal("bytes on the block list by digest were let through whole")
		}
	}
}

// tryGet asks and says what came, where a body that is cut (a small one is cut before any header is sent) is not a failure of the test: code 0.
func (r *rig) tryGet(path string) (int, []byte) {
	req, _ := http.NewRequest("GET", r.ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+r.admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	b, rerr := readAll(resp)
	if rerr != nil {
		return 0, b
	}
	return resp.StatusCode, b
}
