package proxytest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1" //nolint:gosec // dist.shasum of npm
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// tgz makes a tarball as `npm pack` does: the files are under package/.
func tgz(name, version string, extra map[string]string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	files := map[string]string{
		"package/package.json": fmt.Sprintf(`{"name":%q,"version":%q,"main":"index.js"}`, name, version),
		"package/index.js":     "module.exports = 1\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	for n, c := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(c))
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func hashes(b []byte) (string, string) {
	s1 := sha1.Sum(b) //nolint:gosec // dist.shasum
	s5 := sha512.Sum512(b)
	return hex.EncodeToString(s1[:]), "sha512-" + base64.StdEncoding.EncodeToString(s5[:])
}

// publishBody is what `npm publish` sends.
func publishBody(name, version, tag string, data []byte, manifestExtra map[string]any) []byte {
	sh, ig := hashes(data)
	m := map[string]any{"name": name, "version": version, "_id": name + "@" + version, "dist": map[string]string{"shasum": sh, "integrity": ig}, "dependencies": map[string]string{"left-pad": "^1.0.0"}}
	for k, v := range manifestExtra {
		m[k] = v
	}
	file := name + "-" + version + ".tgz" // as npm names it: with the scope
	b, _ := json.Marshal(map[string]any{
		"_id": name, "name": name, "dist-tags": map[string]string{tag: version},
		"versions":     map[string]any{version: m},
		"_attachments": map[string]any{file: map[string]any{"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(data), "length": len(data)}},
	})
	return b
}

func (r *rig) publish(repo, name, version, tag string, data []byte, extra map[string]any) reply {
	r.t.Helper()
	return r.do("PUT", "/repository/"+repo+"/"+strings.ReplaceAll(name, "/", "%2f"), bytes.NewReader(publishBody(name, version, tag, data, extra)), "Content-Type", "application/json")
}

const mimeAbbrev = "application/vnd.npm.install-v1+json"

const abbrevAccept = "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*"

func TestNPMHosted(t *testing.T) {
	r := newRig(t, repos(`{"name": "npm", "format": "npm", "kind": "hosted"}`))
	t1 := tgz("@corp/lib", "1.0.0", nil)
	t2 := tgz("@corp/lib", "1.1.0", map[string]string{"package/npm-shrinkwrap.json": "{}"})
	r.expect(r.publish("npm", "@corp/lib", "1.0.0", "latest", t1, nil), 201)
	r.expect(r.publish("npm", "@corp/lib", "1.1.0", "next", t2, nil), 201)
	r.expect(r.publish("npm", "@corp/lib", "1.0.0", "latest", t1, nil), 200) // the same bytes again
	// Other bytes under a version: refused.
	r.expect(r.publish("npm", "@corp/lib", "1.0.0", "latest", tgz("@corp/lib", "1.0.0", map[string]string{"package/x": "y"}), nil), 409)

	// The abbreviated document, as npm asks for it; the scope is sent encoded or not.
	for _, path := range []string{"/repository/npm/@corp%2flib", "/repository/npm/@corp/lib"} {
		a := r.expect(r.get(path, "Accept", abbrevAccept), 200)
		if a.hd.Get("Content-Type") != mimeAbbrev {
			t.Fatalf("content type %q", a.hd.Get("Content-Type"))
		}
		var d struct {
			Name     string
			DistTags map[string]string `json:"dist-tags"`
			Versions map[string]struct {
				Dist struct{ Tarball, Shasum, Integrity string }
				Deps map[string]string `json:"dependencies"`
				Raw  string            `json:"_id"`
				HRS  bool              `json:"_hasShrinkwrap"`
			}
		}
		if err := json.Unmarshal(a.body, &d); err != nil || d.Name != "@corp/lib" || d.DistTags["latest"] != "1.0.0" || d.DistTags["next"] != "1.1.0" || len(d.Versions) != 2 {
			t.Fatalf("the abbreviated document: %s %v", a.body, err)
		}
		v := d.Versions["1.0.0"]
		sh, ig := hashes(t1)
		if !strings.HasSuffix(v.Dist.Tarball, "/repository/npm/@corp/lib/-/lib-1.0.0.tgz") || !strings.HasPrefix(v.Dist.Tarball, "http://") || v.Dist.Shasum != sh || v.Dist.Integrity != ig || v.Deps["left-pad"] != "^1.0.0" || v.Raw != "" {
			t.Fatalf("a version: %+v", v)
		}
		if !d.Versions["1.1.0"].HRS {
			t.Fatal("the shrinkwrap of the tarball was not noted")
		}
	}
	// The full document has the times and a revision; the ETag answers a repeat with 304.
	full := r.expect(r.get("/repository/npm/@corp%2flib"), 200)
	var fd struct {
		ID   string            `json:"_id"`
		Rev  string            `json:"_rev"`
		Time map[string]string `json:"time"`
	}
	if err := json.Unmarshal(full.body, &fd); err != nil || fd.ID != "@corp/lib" || fd.Rev == "" || fd.Time["1.0.0"] == "" || fd.Time["created"] == "" {
		t.Fatalf("full: %s", full.body)
	}
	r.expect(r.get("/repository/npm/@corp%2flib", "If-None-Match", full.hd.Get("ETag")), 304)
	// The tarball, by the address in the document.
	if f := r.expect(r.get("/repository/npm/@corp/lib/-/lib-1.0.0.tgz"), 200); !bytes.Equal(f.body, t1) {
		t.Fatal("the tarball")
	}
	r.expect(r.get("/repository/npm/@corp/lib/-/lib-9.9.9.tgz"), 404)
	r.expect(r.get("/repository/npm/@corp/lib/-/1.0.0.json"), 404) // what is kept of a version is not an address of a client
	r.expect(r.get("/repository/npm/nothing"), 404)

	// dist-tags: add, move, remove.
	r.expect(r.do("PUT", "/repository/npm/-/package/@corp%2flib/dist-dtags/beta", strings.NewReader(`"1.0.0"`)), 404)
	r.expect(r.do("PUT", "/repository/npm/-/package/@corp%2flib/dist-tags/beta", strings.NewReader(`"1.0.0"`)), 200)
	r.expect(r.do("PUT", "/repository/npm/-/package/@corp%2flib/dist-tags/latest", strings.NewReader(`"1.1.0"`)), 200)
	r.expect(r.do("PUT", "/repository/npm/-/package/@corp%2flib/dist-tags/beta", strings.NewReader(`"7.0.0"`)), 404)
	tags := map[string]string{}
	if err := json.Unmarshal(r.expect(r.get("/repository/npm/-/package/@corp%2flib/dist-tags"), 200).body, &tags); err != nil || tags["latest"] != "1.1.0" || tags["beta"] != "1.0.0" || tags["next"] != "1.1.0" {
		t.Fatalf("tags: %v %v", tags, err)
	}
	r.expect(r.do("DELETE", "/repository/npm/-/package/@corp%2flib/dist-tags/beta", nil), 200)
	r.expect(r.do("DELETE", "/repository/npm/-/package/@corp%2flib/dist-tags/latest", nil), 400)

	// Deprecate: the packument is written back with a message on a version (npm deprecate), with the revision it was read at.
	read := func() (map[string]any, string) {
		var d map[string]any
		_ = json.Unmarshal(r.get("/repository/npm/@corp%2flib?write=true").body, &d)
		return d, d["_rev"].(string)
	}
	d, rv := read()
	d["versions"].(map[string]any)["1.0.0"].(map[string]any)["deprecated"] = "use 1.1"
	put := func(path string, d map[string]any) reply {
		b, _ := json.Marshal(d)
		return r.do("PUT", path, bytes.NewReader(b), "Content-Type", "application/json")
	}
	r.expect(put("/repository/npm/@corp%2flib", d), 200)
	if a := r.get("/repository/npm/@corp%2flib", "Accept", abbrevAccept); !strings.Contains(string(a.body), `"deprecated":"use 1.1"`) {
		t.Fatalf("deprecated: %s", a.body)
	}
	// The same document again carries a revision that is no longer the revision: refused.
	d["_rev"] = rv
	r.expect(put("/repository/npm/@corp%2flib", d), 409)
	// A version with no tarball is refused.
	d, _ = read()
	d["versions"].(map[string]any)["2.0.0"] = map[string]any{"name": "@corp/lib", "version": "2.0.0"}
	r.expect(put("/repository/npm/@corp%2flib", d), 400)

	// Unpublish a version as npm does: the packument without it to the revision, then the tarball.
	d, rv = read()
	delete(d["versions"].(map[string]any), "1.0.0")
	d["dist-tags"] = map[string]string{"latest": "1.1.0", "next": "1.1.0"}
	r.expect(put("/repository/npm/@corp%2flib/-rev/"+rv, d), 200)
	r.expect(r.do("DELETE", "/repository/npm/@corp%2flib/-/lib-1.0.0.tgz/-rev/"+rv, nil), 200)
	r.expect(r.get("/repository/npm/@corp/lib/-/lib-1.0.0.tgz"), 404)
	if a := r.get("/repository/npm/@corp%2flib", "Accept", abbrevAccept); strings.Contains(string(a.body), `"1.0.0"`) {
		t.Fatalf("the version is still there: %s", a.body)
	}
	// The whole package, with a stale revision refused and then with the right one.
	r.expect(r.do("DELETE", "/repository/npm/@corp%2flib/-rev/1-stale", nil), 409)
	_, rv = read()
	r.expect(r.do("DELETE", "/repository/npm/@corp%2flib/-rev/"+rv, nil), 200)
	r.expect(r.get("/repository/npm/@corp%2flib"), 404)
}

func TestNPMPublishRefusals(t *testing.T) {
	r := newRig(t, repos(`{"name": "npm", "format": "npm", "kind": "hosted"}`))
	good := tgz("pkg", "1.0.0", nil)
	// The tarball names another package or version than the publish.
	r.expect(r.publish("npm", "pkg", "1.0.0", "latest", tgz("other", "1.0.0", nil), nil), 400)
	r.expect(r.publish("npm", "pkg", "1.0.0", "latest", tgz("pkg", "1.0.1", nil), nil), 400)
	// The hash in dist is not that of the bytes.
	r.expect(r.publish("npm", "pkg", "1.0.0", "latest", good, map[string]any{"dist": map[string]string{"shasum": strings.Repeat("0", 40)}}), 400)
	// The manifest names another package.
	r.expect(r.publish("npm", "pkg", "1.0.0", "latest", good, map[string]any{"name": "evil"}), 400)
	// Not a tarball; a name in it that leaves the directory; no package.json.
	r.expect(r.publish("npm", "pkg", "1.0.0", "latest", []byte("not a gzip"), nil), 400)
	r.expect(r.publish("npm", "pkg", "1.0.0", "latest", tgz("pkg", "1.0.0", map[string]string{"package/../../etc/x": "x"}), nil), 400)
	r.expect(r.publish("npm", "pkg", "1.0.0", "latest", tgz("pkg", "1.0.0", map[string]string{"/abs": "x"}), nil), 400)
	// Names and versions and tags that npm does not have.
	r.expect(r.publish("npm", "Pkg", "1.0.0", "latest", good, nil), 400)
	r.expect(r.publish("npm", "pkg", "1.0", "latest", good, nil), 400)
	r.expect(r.publish("npm", "pkg", "1.0.0", "1.0.0", good, nil), 400)
	// The name of the address is not the name of the body.
	b := publishBody("pkg", "1.0.0", "latest", good, nil)
	r.expect(r.do("PUT", "/repository/npm/another", bytes.NewReader(b)), 400)
	// Two tarballs, and a body that is not JSON.
	r.expect(r.do("PUT", "/repository/npm/pkg", strings.NewReader("{")), 400)
	// Nothing was kept by any of these.
	r.expect(r.get("/repository/npm/pkg"), 404)
	// A write without a token is 401, before the body is read; a token that may only read is 403.
	req, _ := http.NewRequest("PUT", r.ts.URL+"/repository/npm/pkg", bytes.NewReader(b))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous publish: %d", resp.StatusCode)
	}
	// What is allowed goes through after all that.
	r.expect(r.publish("npm", "pkg", "1.0.0", "latest", good, nil), 201)
	r.expect(r.get("/repository/npm/-/ping"), 200)
	who := r.expect(r.get("/repository/npm/-/whoami"), 200)
	if !strings.Contains(string(who.body), `"username":"root"`) {
		t.Fatalf("whoami: %s", who.body)
	}
	r.expect(r.do("PUT", "/repository/npm/-/user/org.couchdb.user:x", strings.NewReader("{}")), 501)
	r.expect(r.get("/repository/npm/-/npm/v1/security/advisories/bulk"), 404)
}

// newNPMUp makes a false registry of npm: it has packuments (abbreviated) with tarballs at its own address.
type npmUp struct {
	*fake
	mu        sync.Mutex
	docs      map[string]map[string]any
	tars      map[string][]byte
	down      bool
	badTarget map[string]bool // tarball names that are answered with other bytes
}

func newNPMUp(t *testing.T) *npmUp {
	u := &npmUp{docs: map[string]map[string]any{}, tars: map[string][]byte{}, badTarget: map[string]bool{}}
	u.fake = newFake(t, func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.down {
			http.Error(w, "down", 500)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if strings.Contains(p, "/-/") {
			b, ok := u.tars[p]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if u.badTarget[p] {
				b = append([]byte{}, b...)
				b[len(b)/2] ^= 0xff
			}
			_, _ = w.Write(b)
			return
		}
		d, ok := u.docs[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mimeAbbrev)
		_ = json.NewEncoder(w).Encode(d)
	})
	return u
}

// add puts a version in the false registry.
func (u *npmUp) add(name, version string, data []byte, integrity bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, un, scoped := strings.Cut(name, "/")
	if !scoped {
		un = name
	}
	file := un + "-" + version + ".tgz"
	path := name + "/-/" + file
	u.tars[path] = data
	sh, ig := hashes(data)
	dist := map[string]string{"tarball": u.URL + "/" + path, "shasum": sh}
	if integrity {
		dist["integrity"] = ig
	}
	d := u.docs[name]
	if d == nil {
		d = map[string]any{"name": name, "dist-tags": map[string]string{}, "versions": map[string]any{}}
		u.docs[name] = d
	}
	d["versions"].(map[string]any)[version] = map[string]any{"name": name, "version": version, "dist": dist}
	d["dist-tags"].(map[string]string)["latest"] = version
}

func TestNPMProxy(t *testing.T) {
	up := newNPMUp(t)
	data := tgz("left-pad", "1.3.0", nil)
	up.add("left-pad", "1.3.0", data, true)
	up.add("@types/node", "20.1.0", tgz("@types/node", "20.1.0", nil), false) // only a sha1 is named
	set := `{"repositories": [{"name": "pub", "format": "npm", "kind": "proxy", "upstream": {"url": "` + up.URL + `", "allow_private": true, "ttl": "5m"}}], "grants": []}`
	r := newRig(t, set)

	a := r.expect(r.get("/repository/pub/left-pad", "Accept", abbrevAccept), 200)
	var d struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Dist struct{ Tarball, Integrity string }
		}
	}
	if err := json.Unmarshal(a.body, &d); err != nil || d.DistTags["latest"] != "1.3.0" || !strings.HasSuffix(d.Versions["1.3.0"].Dist.Tarball, "/repository/pub/left-pad/-/left-pad-1.3.0.tgz") || d.Versions["1.3.0"].Dist.Integrity == "" {
		t.Fatalf("the document of the proxy: %s %v", a.body, err)
	}
	// The tarball comes through our address, once from the upstream, and is checked against the hash the upstream named.
	for i := 0; i < 3; i++ {
		if f := r.expect(r.get("/repository/pub/left-pad/-/left-pad-1.3.0.tgz"), 200); !bytes.Equal(f.body, data) {
			t.Fatal("the tarball")
		}
	}
	if up.asked("GET", "/left-pad/-/left-pad-1.3.0.tgz") != 1 {
		t.Fatalf("the tarball was asked %d times", up.asked("GET", "/left-pad/-/left-pad-1.3.0.tgz"))
	}
	// A scoped package, with only a sha1 named.
	r.expect(r.get("/repository/pub/@types%2fnode", "Accept", abbrevAccept), 200)
	r.expect(r.get("/repository/pub/@types/node/-/node-20.1.0.tgz"), 200)
	// The packument has a time to live; after it, it is asked again; when the upstream is down it is served stale.
	n := up.asked("GET", "/left-pad")
	r.get("/repository/pub/left-pad")
	if up.asked("GET", "/left-pad") != n {
		t.Fatal("asked inside its time")
	}
	r.advance(6 * time.Minute)
	r.expect(r.get("/repository/pub/left-pad"), 200)
	if up.asked("GET", "/left-pad") != n+1 {
		t.Fatal("not asked after its time")
	}
	up.mu.Lock()
	up.down = true
	up.mu.Unlock()
	r.advance(6 * time.Minute)
	if s := r.expect(r.get("/repository/pub/left-pad"), 200); s.hd.Get("Warning") == "" {
		t.Fatalf("stale: %v", s.hd)
	}
	up.mu.Lock()
	up.down = false
	up.mu.Unlock()
	// A package the upstream lacks, a version it lacks, read-only (the pause after the errors is over first).
	r.advance(time.Minute)
	r.expect(r.get("/repository/pub/nothing"), 404)
	r.expect(r.get("/repository/pub/left-pad/-/left-pad-9.9.9.tgz"), 404)
	r.expect(r.publish("pub", "left-pad", "1.0.0", "latest", data, nil), 405)
}

func TestNPMProxyChecksHashes(t *testing.T) {
	up := newNPMUp(t)
	good := tgz("evil-pkg", "1.0.0", nil)
	up.add("evil-pkg", "1.0.0", good, true)
	up.add("nohash", "1.0.0", tgz("nohash", "1.0.0", nil), false)
	// The upstream's tarball is changed after the packument named its hash: the bytes are not kept and the client does not get a whole body.
	up.badTarget["evil-pkg/-/evil-pkg-1.0.0.tgz"] = true
	// An upstream that names no hash at all.
	delete(up.docs["nohash"]["versions"].(map[string]any)["1.0.0"].(map[string]any)["dist"].(map[string]string), "shasum")
	// A packument that names another package under the name asked.
	up.docs["liar"] = map[string]any{"name": "liar", "dist-tags": map[string]string{"latest": "1.0.0"}, "versions": map[string]any{"1.0.0": map[string]any{"name": "other", "version": "1.0.0", "dist": map[string]string{"tarball": up.URL + "/x.tgz", "shasum": strings.Repeat("a", 40)}}}}
	set := `{"repositories": [{"name": "pub", "format": "npm", "kind": "proxy", "upstream": {"url": "` + up.URL + `", "allow_private": true}}], "grants": []}`
	r := newRig(t, set)
	r.expect(r.get("/repository/pub/evil-pkg"), 200)
	// The body is cut one byte short, or the answer does not come at all (a small body has no header sent yet when it is cut): not the whole of the bytes.
	req, _ := http.NewRequest("GET", r.ts.URL+"/repository/pub/evil-pkg/-/evil-pkg-1.0.0.tgz", nil)
	req.Header.Set("Authorization", "Bearer "+r.admin)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		b, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr == nil && resp.StatusCode == 200 && bytes.Equal(b, good) {
			t.Fatal("bytes that do not have the hash were let through whole")
		}
	}
	if r.auditCount("upstream_changed", "digest") == 0 {
		t.Fatal("no alarm of other bytes")
	}
	// Nothing was kept: when the upstream is right again, the tarball is fetched and kept.
	up.mu.Lock()
	up.badTarget["evil-pkg/-/evil-pkg-1.0.0.tgz"] = false
	up.mu.Unlock()
	r.advance(time.Hour)
	if f := r.get("/repository/pub/evil-pkg/-/evil-pkg-1.0.0.tgz"); f.code != 200 || !bytes.Equal(f.body, good) {
		t.Fatalf("after: %d", f.code)
	}
	r.expect(r.get("/repository/pub/nohash"), 200)
	if a := r.get("/repository/pub/nohash/-/nohash-1.0.0.tgz"); a.code == 200 {
		t.Fatal("a tarball with no hash named was kept")
	}
	r.expect(r.get("/repository/pub/liar"), 404) // its only version is not this package
}

func TestNPMGroup(t *testing.T) {
	up := newNPMUp(t)
	up.add("left-pad", "1.3.0", tgz("left-pad", "1.3.0", nil), true)
	up.add("corp-lib", "9.9.9", tgz("corp-lib", "9.9.9", nil), true) // the public upstream has a package with the name of ours
	up.add("@corp/ui", "9.9.9", tgz("@corp/ui", "9.9.9", nil), true)
	up.add("Corp-Lib", "9.9.9", tgz("Corp-Lib", "9.9.9", nil), true) // the same name with capitals: an old name that the upstream has
	set := `{"repositories": [
		{"name": "mine", "format": "npm", "kind": "hosted"},
		{"name": "pub", "format": "npm", "kind": "proxy", "upstream": {"url": "` + up.URL + `", "allow_private": true}},
		{"name": "all", "format": "npm", "kind": "group", "members": ["pub", "mine"], "write_to": "mine", "claims": ["@corp/"]}]
	, "grants": []}`
	r := newRig(t, set)
	own := tgz("corp-lib", "1.0.0", nil)
	r.expect(r.publish("all", "corp-lib", "1.0.0", "latest", own, nil), 201) // to write_to
	r.expect(r.get("/repository/mine/corp-lib"), 200)
	// The public package comes from the proxy, through the group; the group's address is in the tarball's address.
	a := r.expect(r.get("/repository/all/left-pad", "Accept", abbrevAccept), 200)
	if !strings.Contains(string(a.body), "/repository/all/left-pad/-/left-pad-1.3.0.tgz") {
		t.Fatalf("the group's document: %s", a.body)
	}
	r.expect(r.get("/repository/all/left-pad/-/left-pad-1.3.0.tgz"), 200)
	// A name of ours is ours although the proxy comes first and has the same name with a higher version (dependency confusion).
	c := r.expect(r.get("/repository/all/corp-lib", "Accept", abbrevAccept), 200)
	if strings.Contains(string(c.body), "9.9.9") {
		t.Fatalf("the public version got in: %s", c.body)
	}
	if f := r.expect(r.get("/repository/all/corp-lib/-/corp-lib-1.0.0.tgz"), 200); !bytes.Equal(f.body, own) {
		t.Fatal("our tarball")
	}
	r.expect(r.get("/repository/all/corp-lib/-/corp-lib-9.9.9.tgz"), 404)
	// A scope that the group claims is not asked of the proxy, though nobody has published it.
	before := up.count()
	r.expect(r.get("/repository/all/@corp%2fui"), 404)
	if up.count() != before {
		t.Fatal("the proxy was asked for a claimed name")
	}
	// A package that is claimed by being published stays claimed after it is deleted.
	d := map[string]any{}
	_ = json.Unmarshal(r.get("/repository/mine/corp-lib").body, &d)
	r.expect(r.do("DELETE", "/repository/mine/corp-lib/-rev/"+d["_rev"].(string), nil), 200)
	r.expect(r.get("/repository/all/corp-lib"), 404)
	// Capitals do not get round a claim.
	r.expect(r.get("/repository/all/Corp-Lib"), 404)
	// A group with no write_to refuses a publish.
	r.expect(r.get("/repository/all/-/ping"), 200)
}
