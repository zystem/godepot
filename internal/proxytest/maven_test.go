package proxytest

import (
	"bytes"
	"crypto/md5"  //nolint:gosec // maven's md5
	"crypto/sha1" //nolint:gosec // maven's sha1
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func mpom(g, a, v string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0"?><project><modelVersion>4.0.0</modelVersion><groupId>%s</groupId><artifactId>%s</artifactId><version>%s</version></project>`, g, a, v))
}

func sha1hex(b []byte) string { s := sha1.Sum(b); return hex.EncodeToString(s[:]) } //nolint:gosec
func md5hex(b []byte) string  { s := md5.Sum(b); return hex.EncodeToString(s[:]) }  //nolint:gosec
func sha512hex(b []byte) string {
	s := sha512.Sum512(b)
	return hex.EncodeToString(s[:])
}

func (r *rig) put(path string, b []byte) reply {
	r.t.Helper()
	return r.do("PUT", path, bytes.NewReader(b))
}

// deploy sends a file and its checksums as `mvn deploy` does.
func (r *rig) deploy(base, path string, b []byte) {
	r.t.Helper()
	r.expect(r.put("/repository/"+base+"/"+path, b), 201)
	r.expect(r.put("/repository/"+base+"/"+path+".sha1", []byte(sha1hex(b))), 201)
	r.expect(r.put("/repository/"+base+"/"+path+".md5", []byte(md5hex(b))), 201)
}

func TestMavenHosted(t *testing.T) {
	r := newRig(t, repos(`{"name": "mvn", "format": "maven", "kind": "hosted"}`))
	jar := blobBytes(2000, 1)
	for _, v := range []string{"1.0", "1.2", "1.10", "1.1"} {
		r.deploy("mvn", "com/acme/app/"+v+"/app-"+v+".pom", mpom("com.acme", "app", v))
		r.deploy("mvn", "com/acme/app/"+v+"/app-"+v+".jar", append(jar, v...))
	}
	r.expect(r.put("/repository/mvn/com/acme/app/1.0/app-1.0.jar", append(jar, "1.0"...)), 200) // the same bytes again
	r.expect(r.put("/repository/mvn/com/acme/app/1.0/app-1.0.jar", []byte("other")), 409)       // a release is not overwritten
	want := append(jar, "1.2"...)
	if f := r.expect(r.get("/repository/mvn/com/acme/app/1.2/app-1.2.jar"), 200); !bytes.Equal(f.body, want) {
		t.Fatal("the jar")
	}
	// The checksum files are made by the program.
	for algo, f := range map[string]func([]byte) string{"sha1": sha1hex, "md5": md5hex, "sha256": sum, "sha512": sha512hex} {
		if g := r.expect(r.get("/repository/mvn/com/acme/app/1.2/app-1.2.jar."+algo), 200); string(g.body) != f(want) {
			t.Fatalf("%s: %s", algo, g.body)
		}
	}
	r.expect(r.get("/repository/mvn/com/acme/app/1.2/app-9.9.jar.sha1"), 404)
	// A checksum that is not the file's is refused; one before its file is refused.
	r.expect(r.put("/repository/mvn/com/acme/app/1.2/app-1.2.jar.sha1", []byte(strings.Repeat("0", 40))), 400)
	r.expect(r.put("/repository/mvn/com/acme/app/3.0/app-3.0.jar.sha1", []byte(sha1hex(nil))), 409)
	r.expect(r.put("/repository/mvn/com/acme/app/1.2/app-1.2.jar.sha1", []byte(sha1hex(want)+"  app-1.2.jar\n")), 201) // a checksum file may name the file

	// The metadata is made from the files, with the versions in Maven's order.
	md := string(r.expect(r.get("/repository/mvn/com/acme/app/maven-metadata.xml"), 200).body)
	for _, s := range []string{"<groupId>com.acme</groupId>", "<artifactId>app</artifactId>", "<latest>1.10</latest>", "<release>1.10</release>",
		"<version>1.0</version>\n      <version>1.1</version>\n      <version>1.2</version>\n      <version>1.10</version>", "<lastUpdated>"} {
		if !strings.Contains(md, s) {
			t.Fatalf("metadata lacks %q:\n%s", s, md)
		}
	}
	if g := r.expect(r.get("/repository/mvn/com/acme/app/maven-metadata.xml.sha1"), 200); string(g.body) != sha1hex([]byte(md)) {
		t.Fatalf("the sha1 of the metadata: %s", g.body)
	}
	// A metadata that a client sends is taken and not kept.
	r.expect(r.put("/repository/mvn/com/acme/app/maven-metadata.xml", []byte("<metadata><versioning><latest>99</latest></versioning></metadata>")), 201)
	if string(r.get("/repository/mvn/com/acme/app/maven-metadata.xml").body) != md {
		t.Fatal("an uploaded metadata was kept")
	}
	r.expect(r.get("/repository/mvn/com/acme/nothing/maven-metadata.xml"), 404)
	// A pom that is not the pom of its path is refused; a parent may give the group and the version.
	r.expect(r.put("/repository/mvn/com/acme/app/4.0/app-4.0.pom", mpom("com.acme", "other", "4.0")), 400)
	r.expect(r.put("/repository/mvn/com/acme/app/4.0/app-4.0.pom", mpom("com.evil", "app", "4.0")), 400)
	r.expect(r.put("/repository/mvn/com/acme/app/4.0/app-4.0.pom", mpom("com.acme", "app", "4.1")), 400)
	r.expect(r.put("/repository/mvn/com/acme/app/4.0/app-4.0.pom", []byte("not xml <")), 400)
	child := `<project><parent><groupId>com.acme</groupId><artifactId>parent</artifactId><version>5.0</version></parent><artifactId>app</artifactId></project>`
	r.expect(r.put("/repository/mvn/com/acme/app/5.0/app-5.0.pom", []byte(child)), 201)
	// A path that is not Maven's.
	r.expect(r.put("/repository/mvn/com/acme/app/1.0/readme.txt", []byte("x")), 400)
	r.expect(r.put("/repository/mvn/x.jar", []byte("x")), 400)
	// A signature is a file like another, with checksums.
	r.deploy("mvn", "com/acme/app/1.2/app-1.2.jar.asc", []byte("-----BEGIN PGP SIGNATURE-----"))
	// A file can be deleted.
	r.expect(r.do("DELETE", "/repository/mvn/com/acme/app/1.2/app-1.2.jar", nil), 204)
	r.expect(r.get("/repository/mvn/com/acme/app/1.2/app-1.2.jar"), 404)
	// An anonymous write is 401.
	req, _ := http.NewRequest("PUT", r.ts.URL+"/repository/mvn/com/acme/app/6.0/app-6.0.jar", strings.NewReader("x"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}
}

func TestMavenSnapshots(t *testing.T) {
	r := newRig(t, repos(`{"name": "mvn", "format": "maven", "kind": "hosted"}`))
	d := "com/acme/lib/2.0-SNAPSHOT/"
	r.deploy("mvn", d+"lib-2.0-20240101.100000-1.jar", []byte("build one"))
	r.deploy("mvn", d+"lib-2.0-20240101.100000-1.pom", mpom("com.acme", "lib", "2.0-SNAPSHOT"))
	r.deploy("mvn", d+"lib-2.0-20240102.110000-2.jar", []byte("build two"))
	r.deploy("mvn", d+"lib-2.0-20240102.110000-2-sources.jar", []byte("sources two"))
	// A file with no timestamp is not taken: snapshots are unique.
	r.expect(r.put("/repository/mvn/"+d+"lib-2.0-SNAPSHOT.jar", []byte("x")), 400)
	md := string(r.expect(r.get("/repository/mvn/"+d+"maven-metadata.xml"), 200).body)
	for _, s := range []string{"<version>2.0-SNAPSHOT</version>", "<timestamp>20240102.110000</timestamp>", "<buildNumber>2</buildNumber>",
		"<extension>jar</extension>\n        <value>2.0-20240102.110000-2</value>", "<classifier>sources</classifier>", "<extension>pom</extension>\n        <value>2.0-20240101.100000-1</value>"} {
		if !strings.Contains(md, s) {
			t.Fatalf("snapshot metadata lacks %q:\n%s", s, md)
		}
	}
	// The artifact's metadata lists the snapshot as latest and has no release.
	am := string(r.expect(r.get("/repository/mvn/com/acme/lib/maven-metadata.xml"), 200).body)
	if !strings.Contains(am, "<latest>2.0-SNAPSHOT</latest>") || strings.Contains(am, "<release>") {
		t.Fatalf("artifact metadata:\n%s", am)
	}
	// A snapshot's pom is checked against the snapshot version.
	r.expect(r.put("/repository/mvn/"+d+"lib-2.0-20240103.100000-3.pom", mpom("com.acme", "lib", "2.0")), 400)
}

// newMavenUp is a false Maven repository.
type mavenUp struct {
	*fake
	mu    sync.Mutex
	files map[string][]byte
	down  bool
}

func newMavenUp(t *testing.T) *mavenUp {
	u := &mavenUp{files: map[string][]byte{}}
	u.fake = newFake(t, func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.down {
			http.Error(w, "down", 500)
			return
		}
		b, ok := u.files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	})
	return u
}

func (u *mavenUp) add(path string, b []byte, sums ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.files[path] = b
	for _, a := range sums {
		switch a {
		case "sha1":
			u.files[path+".sha1"] = []byte(sha1hex(b))
		case "sha256":
			u.files[path+".sha256"] = []byte(sum(b))
		case "sha512":
			u.files[path+".sha512"] = []byte(sha512hex(b))
		}
	}
}

func TestMavenProxy(t *testing.T) {
	up := newMavenUp(t)
	jar := blobBytes(3000, 4)
	up.add("org/lib/core/1.0/core-1.0.jar", jar, "sha1")
	up.add("org/lib/core/1.0/core-1.0.pom", mpom("org.lib", "core", "1.0"), "sha1", "sha512")
	up.add("org/lib/core/maven-metadata.xml", []byte("<metadata><versioning><latest>1.0</latest></versioning></metadata>"), "sha1")
	set := `{"repositories": [{"name": "central", "format": "maven", "kind": "proxy", "upstream": {"url": "` + up.URL + `", "allow_private": true, "ttl": "5m"}}], "grants": []}`
	r := newRig(t, set)
	for i := 0; i < 3; i++ {
		if f := r.expect(r.get("/repository/central/org/lib/core/1.0/core-1.0.jar"), 200); !bytes.Equal(f.body, jar) {
			t.Fatal("the jar")
		}
	}
	if up.asked("GET", "/org/lib/core/1.0/core-1.0.jar") != 1 {
		t.Fatalf("the jar was asked %d times", up.asked("GET", "/org/lib/core/1.0/core-1.0.jar"))
	}
	// The checksum file of the upstream is kept and served; the strongest one is the one checked (the pom has a sha512).
	if g := r.expect(r.get("/repository/central/org/lib/core/1.0/core-1.0.jar.sha1"), 200); string(g.body) != sha1hex(jar) {
		t.Fatalf("sha1: %s", g.body)
	}
	r.expect(r.get("/repository/central/org/lib/core/1.0/core-1.0.pom"), 200)
	if up.asked("GET", "/org/lib/core/1.0/core-1.0.pom.sha512") != 1 || up.asked("GET", "/org/lib/core/1.0/core-1.0.pom.sha1") != 0 {
		t.Fatal("the sha512 was not the checksum used")
	}
	// The metadata has a time to live; its checksum is made of the copy that is held.
	md := r.expect(r.get("/repository/central/org/lib/core/maven-metadata.xml"), 200).body
	// The upstream's metadata changes (and its checksum file with it) while the copy is inside its time: the checksum that is served is that of the copy.
	up.add("org/lib/core/maven-metadata.xml", []byte("<metadata>changed under the copy</metadata>"), "sha1")
	if g := r.expect(r.get("/repository/central/org/lib/core/maven-metadata.xml.sha1"), 200); string(g.body) != sha1hex(md) {
		t.Fatalf("the checksum of the metadata: %s", g.body)
	}
	n := up.asked("GET", "/org/lib/core/maven-metadata.xml")
	r.get("/repository/central/org/lib/core/maven-metadata.xml")
	if up.asked("GET", "/org/lib/core/maven-metadata.xml") != n {
		t.Fatal("the metadata was asked inside its time")
	}
	up.add("org/lib/core/maven-metadata.xml", []byte("<metadata>newer</metadata>"), "sha1")
	r.advance(6 * time.Minute)
	if g := r.expect(r.get("/repository/central/org/lib/core/maven-metadata.xml"), 200); !strings.Contains(string(g.body), "newer") {
		t.Fatal("the metadata was not asked again after its time")
	}
	up.mu.Lock()
	up.down = true
	up.mu.Unlock()
	r.advance(6 * time.Minute)
	if s := r.expect(r.get("/repository/central/org/lib/core/maven-metadata.xml"), 200); s.hd.Get("Warning") == "" {
		t.Fatal("not stale")
	}
	up.mu.Lock()
	up.down = false
	up.mu.Unlock()
	r.advance(time.Minute)
	r.expect(r.get("/repository/central/org/lib/nothing/1.0/nothing-1.0.jar"), 404)
	r.expect(r.put("/repository/central/org/lib/core/1.0/core-1.0.jar", jar), 405)
}

func TestMavenProxyChecksHashes(t *testing.T) {
	up := newMavenUp(t)
	good := blobBytes(4000, 6)
	up.add("o/bad/1.0/bad-1.0.jar", good, "sha1")
	// The upstream has other bytes than its checksum names.
	up.files["o/bad/1.0/bad-1.0.jar"] = blobBytes(4000, 7)
	// A file that has no checksum file at all.
	up.add("o/none/1.0/none-1.0.jar", good)
	set := `{"repositories": [{"name": "c", "format": "maven", "kind": "proxy", "upstream": {"url": "` + up.URL + `", "allow_private": true}}], "grants": []}`
	r := newRig(t, set)
	req, _ := http.NewRequest("GET", r.ts.URL+"/repository/c/o/bad/1.0/bad-1.0.jar", nil)
	req.Header.Set("Authorization", "Bearer "+r.admin)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		b, rerr := readAll(resp)
		if rerr == nil && resp.StatusCode == 200 && bytes.Equal(b, up.files["o/bad/1.0/bad-1.0.jar"]) {
			t.Fatal("bytes that do not have the checksum were let through whole")
		}
	}
	if r.auditCount("upstream_changed", "digest") == 0 {
		t.Fatal("no alarm of other bytes")
	}
	// Nothing was kept: once the upstream is right, the file is fetched and kept.
	up.add("o/bad/1.0/bad-1.0.jar", good, "sha1")
	r.advance(time.Hour)
	if f := r.get("/repository/c/o/bad/1.0/bad-1.0.jar"); f.code != 200 || !bytes.Equal(f.body, good) {
		t.Fatalf("after: %d", f.code)
	}
	r.expect(r.get("/repository/c/o/none/1.0/none-1.0.jar"), 404)
}

func TestMavenGroup(t *testing.T) {
	up := newMavenUp(t)
	up.add("org/lib/core/1.0/core-1.0.jar", []byte("public core"), "sha1")
	up.add("org/lib/core/maven-metadata.xml", []byte("<metadata>public</metadata>"), "sha1")
	up.add("com/acme/app/99.0/app-99.0.jar", []byte("the public one"), "sha1") // a name of ours on the public repository, with a higher version
	up.add("com/acme/app/maven-metadata.xml", []byte("<metadata><versioning><latest>99.0</latest></versioning></metadata>"), "sha1")
	up.add("com/acme/secret/1.0/secret-1.0.jar", []byte("x"), "sha1")
	set := `{"repositories": [
		{"name": "mine", "format": "maven", "kind": "hosted"},
		{"name": "central", "format": "maven", "kind": "proxy", "upstream": {"url": "` + up.URL + `", "allow_private": true}},
		{"name": "all", "format": "maven", "kind": "group", "members": ["central", "mine"], "write_to": "mine", "claims": ["com/acme/"]}],
		"grants": []}`
	r := newRig(t, set)
	r.deploy("all", "com/acme/app/1.0/app-1.0.jar", []byte("our jar")) // to write_to
	r.expect(r.get("/repository/mine/com/acme/app/1.0/app-1.0.jar"), 200)
	if f := r.expect(r.get("/repository/all/org/lib/core/1.0/core-1.0.jar"), 200); string(f.body) != "public core" {
		t.Fatal("the public jar through the group")
	}
	// Dependency confusion: the public 99.0 does not get in, in the metadata or as a file.
	if md := r.expect(r.get("/repository/all/com/acme/app/maven-metadata.xml"), 200); strings.Contains(string(md.body), "99.0") || !strings.Contains(string(md.body), "<latest>1.0</latest>") {
		t.Fatalf("metadata: %s", md.body)
	}
	r.expect(r.get("/repository/all/com/acme/app/99.0/app-99.0.jar"), 404)
	// A claimed prefix that nobody has published is not asked.
	before := up.count()
	r.expect(r.get("/repository/all/com/acme/secret/1.0/secret-1.0.jar"), 404)
	if up.count() != before {
		t.Fatal("the proxy was asked for a claimed name")
	}
	// Our jar stays claimed after it is deleted.
	r.expect(r.do("DELETE", "/repository/mine/com/acme/app/1.0/app-1.0.jar", nil), 204)
	r.expect(r.get("/repository/all/com/acme/app/1.0/app-1.0.jar"), 404)
}

func readAll(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
