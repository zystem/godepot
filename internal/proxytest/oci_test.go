package proxytest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// image is a small image for the false registry: a config, a layer, and the manifest that names them.
type image struct {
	config, layer, manifest []byte
}

func (i *image) digest() string  { return "sha256:" + sum(i.manifest) }
func (i *image) cdigest() string { return "sha256:" + sum(i.config) }
func (i *image) ldigest() string { return "sha256:" + sum(i.layer) }

func newImage(layerSize int, seed byte) *image {
	i := &image{config: []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","seed":%d}`, seed)), layer: blobBytes(layerSize, seed)}
	m := map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": i.cdigest(), "size": len(i.config)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": i.ldigest(), "size": len(i.layer)}},
	}
	i.manifest, _ = json.Marshal(m)
	return i
}

// registry is a false registry of the distribution API, in memory, that can ask for a token.
type registry struct {
	mu       sync.Mutex
	images   map[string]map[string]*image // name -> tag -> image
	down     bool
	tokenURL string // when set, the registry asks for a bearer token from this service
	blobHook func(w http.ResponseWriter, r *http.Request) bool
}

func (g *registry) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		down, tokenURL, hook := g.down, g.tokenURL, g.blobHook
		g.mu.Unlock()
		if down {
			w.WriteHeader(500)
			return
		}
		if tokenURL != "" && r.Header.Get("Authorization") != "Bearer good-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="fake",scope="repository:x:pull"`, tokenURL+"/token"))
			w.WriteHeader(401)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/v2/")
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(200)
		case strings.Contains(p, "/manifests/"):
			name, ref, _ := strings.Cut(p, "/manifests/")
			var im *image
			for tag, c := range g.images[name] {
				if tag == ref || c.digest() == ref {
					im = c
				}
			}
			if im == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", im.digest())
			w.Header().Set("Content-Length", strconv.Itoa(len(im.manifest)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(im.manifest)
			}
		case strings.Contains(p, "/blobs/"):
			name, d, _ := strings.Cut(p, "/blobs/")
			if hook != nil {
				g.mu.Unlock()
				done := hook(w, r)
				g.mu.Lock()
				if done {
					return
				}
			}
			for _, im := range g.images[name] {
				for _, b := range [][]byte{im.config, im.layer} {
					if "sha256:"+sum(b) == d {
						w.Header().Set("Content-Length", strconv.Itoa(len(b)))
						if r.Method != http.MethodHead {
							_, _ = w.Write(b)
						}
						return
					}
				}
			}
			http.NotFound(w, r)
		case strings.HasSuffix(p, "/tags/list"):
			name := strings.TrimSuffix(p, "/tags/list")
			tags, ok := g.images[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			list := []string{}
			for t := range tags {
				list = append(list, t)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": list})
		default:
			http.NotFound(w, r)
		}
	}
}

func proxyOCI(f *fake, more string) string {
	return repos(conf(`{"name": "up", "format": "oci", "kind": "proxy", "upstream": {"url": %q, "allow_private": true%s}}`, f.URL, more))
}

// pull reads an image from a name the way a client does: the manifest by tag, then the config and the layer.
func (r *rig) pull(name, tag string, im *image) {
	r.t.Helper()
	m := r.expect(r.get("/v2/"+name+"/manifests/"+tag, "Accept", "application/vnd.oci.image.manifest.v1+json"), 200)
	if !bytes.Equal(m.body, im.manifest) || m.hd.Get("Docker-Content-Digest") != im.digest() {
		r.t.Fatalf("the manifest: %v", m.hd)
	}
	for _, b := range [][]byte{im.config, im.layer} {
		got := r.expect(r.get("/v2/"+name+"/blobs/sha256:"+sum(b)), 200)
		if !bytes.Equal(got.body, b) {
			r.t.Fatalf("a blob of %d bytes came back as %d bytes", len(b), len(got.body))
		}
	}
}

// P1, P3: an image is pulled through the proxy; everything that is named by a digest is kept and not asked again; a tag is trusted for its time to live.
func TestOCIProxyPullsAndKeeps(t *testing.T) {
	im := newImage(300000, 1)
	reg := &registry{images: map[string]map[string]*image{"library/alpine": {"3.20": im}}}
	f := newFake(t, reg.handler())
	r := newRig(t, proxyOCI(f, ""))
	r.pull("up/library/alpine", "3.20", im)
	asked := f.count()
	// By digest, the manifest and the blobs are the cache's; the tag is fresh: nothing goes upstream.
	r.pull("up/library/alpine", "3.20", im)
	r.expect(r.get("/v2/up/library/alpine/manifests/"+im.digest()), 200)
	if f.count() != asked {
		t.Fatalf("the cache was not enough: %d -> %d asks (%v)", asked, f.count(), f.seen)
	}
	// A range of a blob from the cache, and a HEAD.
	p := r.expect(r.get("/v2/up/library/alpine/blobs/"+im.ldigest(), "Range", "bytes=5-14"), 206)
	if !bytes.Equal(p.body, im.layer[5:15]) {
		t.Fatal("range")
	}
	r.expect(r.do("HEAD", "/v2/up/library/alpine/blobs/"+im.ldigest(), nil), 200)
	// Read-only.
	r.expect(r.do("PUT", "/v2/up/library/alpine/manifests/x", bytes.NewReader(im.manifest), "Content-Type", "application/vnd.oci.image.manifest.v1+json"), 405)
	r.expect(r.do("POST", "/v2/up/library/alpine/blobs/uploads/", nil), 405)
	// Nothing that is not the upstream's: a name or a blob it lacks.
	r.expect(r.get("/v2/up/library/none/manifests/1"), 404)
	r.expect(r.get("/v2/up/library/alpine/blobs/sha256:"+sum([]byte("not there"))), 404)
	// The list of tags is the upstream's.
	l := r.expect(r.get("/v2/up/library/alpine/tags/list"), 200)
	if !bytes.Contains(l.body, []byte(`"3.20"`)) || !bytes.Contains(l.body, []byte(`"up/library/alpine"`)) {
		t.Fatalf("tags: %s", l.body)
	}
}

// T15: a tag that is past its time to live is checked with a HEAD, not fetched again; a tag that has moved is fetched; the upstream's rate limit is spared.
func TestOCIProxyChecksTagsWithHead(t *testing.T) {
	v1, v2 := newImage(1000, 1), newImage(1000, 2)
	reg := &registry{images: map[string]map[string]*image{"app": {"latest": v1}}}
	f := newFake(t, reg.handler())
	r := newRig(t, proxyOCI(f, `, "ttl": "5m"`))
	r.pull("up/app", "latest", v1)
	r.advance(6 * time.Minute)
	gets := f.asked("GET", "/v2/app/manifests/latest")
	r.pull("up/app", "latest", v1)
	if f.asked("HEAD", "/v2/app/manifests/latest") != 1 || f.asked("GET", "/v2/app/manifests/latest") != gets {
		t.Fatalf("the tag was not checked with one HEAD: %v", f.seen)
	}
	// Moved upstream: the next check finds it, and the manifest is fetched once.
	reg.mu.Lock()
	reg.images["app"]["latest"] = v2
	reg.mu.Unlock()
	r.advance(6 * time.Minute)
	r.pull("up/app", "latest", v2)
	// The old one is still there by its digest.
	r.expect(r.get("/v2/up/app/manifests/"+v1.digest()), 200)
	// Down: the tag that is past its time is served as it was, marked stale (P3).
	reg.mu.Lock()
	reg.down = true
	reg.mu.Unlock()
	r.advance(6 * time.Minute)
	a := r.expect(r.get("/v2/up/app/manifests/latest"), 200)
	if a.hd.Get("Docker-Content-Digest") != v2.digest() || a.hd.Get("Warning") == "" || r.stale.Value("up") != 1 {
		t.Fatalf("stale: %v", a.hd)
	}
	// What was never cached is not served while it is down.
	if b := r.get("/v2/up/app/manifests/other"); b.code != 503 {
		t.Fatalf("never cached while down: %d", b.code)
	}
}

// P4: a "not found" is remembered per name; an error never is.
func TestOCIProxyNegativeCache(t *testing.T) {
	reg := &registry{images: map[string]map[string]*image{}}
	f := newFake(t, reg.handler())
	r := newRig(t, proxyOCI(f, ""))
	for i := 0; i < 3; i++ {
		r.expect(r.get("/v2/up/app/manifests/new"), 404)
	}
	if f.count() != 1 {
		t.Fatalf("asked %d times", f.count())
	}
	im := newImage(100, 3)
	reg.mu.Lock()
	reg.images["app"] = map[string]*image{"new": im}
	reg.mu.Unlock()
	r.expect(r.get("/v2/up/app/manifests/new"), 404) // still remembered
	r.expect(r.do("POST", "/api/v1/repositories/up/forget", nil), 200)
	r.pull("up/app", "new", im)
}

// P2, T15: 100 clients ask one cold layer: one fetch from the registry, and every client gets the bytes.
func TestOCIProxyJoinsFetchesOfOneLayer(t *testing.T) {
	im := newImage(2<<20, 4)
	reg := &registry{images: map[string]map[string]*image{"big": {"v": im}}}
	reg.blobHook = func(w http.ResponseWriter, r *http.Request) bool { time.Sleep(400 * time.Millisecond); return false }
	f := newFake(t, reg.handler())
	r := newRig(t, proxyOCI(f, ""))
	r.expect(r.get("/v2/up/big/manifests/v"), 200)
	var wg sync.WaitGroup
	bad := make(chan string, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := r.do("GET", "/v2/up/big/blobs/"+im.ldigest(), nil)
			if a.code != 200 || !bytes.Equal(a.body, im.layer) || a.err != nil {
				bad <- fmt.Sprintf("%d %v", a.code, a.err)
			}
		}()
	}
	wg.Wait()
	close(bad)
	for b := range bad {
		t.Fatalf("a client got %s", b)
	}
	if n := f.asked("GET", "/v2/big/blobs/"+im.ldigest()); n != 1 {
		t.Fatalf("100 clients made %d fetches of the layer, want 1", n)
	}
}

// P5: a blob whose bytes are not the digest of its address is not kept, and the client's body is cut.
func TestOCIProxyDoesNotKeepOtherBytes(t *testing.T) {
	im := newImage(100000, 5)
	reg := &registry{images: map[string]map[string]*image{"app": {"v": im}}}
	reg.blobHook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, im.ldigest()) {
			w.Header().Set("Content-Length", strconv.Itoa(len(im.layer)))
			_, _ = w.Write(blobBytes(len(im.layer), 99)) // other bytes under the digest
			return true
		}
		return false
	}
	f := newFake(t, reg.handler())
	r := newRig(t, proxyOCI(f, ""))
	r.expect(r.get("/v2/up/app/manifests/v"), 200)
	a := r.do("GET", "/v2/up/app/blobs/"+im.ldigest(), nil)
	if a.err == nil && a.code == 200 && len(a.body) == len(im.layer) {
		t.Fatal("other bytes were given whole")
	}
	if r.changed.Value("up") != 1 {
		t.Fatal("not counted")
	}
	if b := r.do("GET", "/v2/up/app/blobs/"+im.ldigest(), nil); b.code == 200 && len(b.body) == len(im.layer) && bytes.Equal(b.body, blobBytes(len(im.layer), 99)) {
		t.Fatal("kept")
	}
	// The manifest by a digest that its bytes do not have is refused too.
	reg.mu.Lock()
	reg.images["app"]["w"] = &image{manifest: im.manifest, config: im.config, layer: im.layer}
	reg.mu.Unlock()
}

// P8: the token service of a registry is asked; credentials go to it only when it is on the registry's host or on token_hosts.
func TestOCIProxyBearerTokenService(t *testing.T) {
	t.Setenv("TEST_OCI_PASSWORD", "hunter22")
	im := newImage(1000, 6)
	reg := &registry{images: map[string]map[string]*image{"app": {"v": im}}}
	tok := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "good-token", "expires_in": 300})
	})
	f := newFake(t, reg.handler())
	// The token service is at "localhost", the registry at 127.0.0.1: another host.
	reg.tokenURL = "http://localhost:" + tok.URL[strings.LastIndex(tok.URL, ":")+1:]
	r := newRig(t, proxyOCI(f, `, "username": "bot", "password_env": "TEST_OCI_PASSWORD"`))
	r.pull("up/app", "v", im)
	for _, h := range tok.auth {
		if h != "" {
			t.Fatalf("the token service of another host was sent credentials: %q", h)
		}
	}
	// With the host named, the service gets them.
	tok.mu.Lock()
	tok.auth = nil
	tok.mu.Unlock()
	r2 := newRig(t, proxyOCI(f, `, "username": "bot", "password_env": "TEST_OCI_PASSWORD", "token_hosts": ["localhost"]`))
	r2.pull("up/app", "v", im)
	tok.mu.Lock()
	defer tok.mu.Unlock()
	if len(tok.auth) == 0 || !strings.HasPrefix(tok.auth[0], "Basic ") {
		t.Fatalf("the listed token service saw %q", tok.auth)
	}
	_ = url.QueryEscape
}

const groupOCI = `{"name": "mine", "format": "oci", "kind": "hosted"},
  {"name": "up", "format": "oci", "kind": "proxy", "upstream": {"url": %q, "allow_private": true}},
  {"name": "all", "format": "oci", "kind": "group", "members": ["up", "mine"], "write_to": "mine", "claims": ["corp/"]},
  {"name": "ro", "format": "oci", "kind": "group", "members": ["mine"]}`

// R1, R2, R3, T20: one address for hosted and proxy. A push to the group goes to write_to and its Location names the group; the pull is answered by the first member that has the image; an image of
// ours that the upstream also has is ours, and the upstream is not asked for it; a prefix that is claimed is not fetched.
func TestOCIGroup(t *testing.T) {
	ours, theirs := newImage(5000, 7), newImage(5000, 8)
	pub := newImage(8000, 9)
	reg := &registry{images: map[string]map[string]*image{
		"corp/app":       {"v1": theirs}, // the same name upstream, with other bytes: the confusion
		"corp/secret":    {"v1": theirs},
		"library/alpine": {"3": pub},
	}}
	f := newFake(t, reg.handler())
	r := newRig(t, `{"repositories": [`+conf(groupOCI, f.URL)+`], "grants": []}`)

	// Push to the group: a blob in one request, then the manifest by tag.
	for _, b := range [][]byte{ours.config, ours.layer} {
		a := r.expect(r.do("POST", "/v2/all/corp/app/blobs/uploads/?digest=sha256:"+sum(b), bytes.NewReader(b)), 201)
		if !strings.HasPrefix(a.hd.Get("Location"), "/v2/all/corp/app/blobs/") {
			t.Fatalf("the Location names a member, not the group: %q", a.hd.Get("Location"))
		}
	}
	// And a part upload, to see that the session's address is the group's.
	s := r.expect(r.do("POST", "/v2/all/corp/app/blobs/uploads/", nil), 202)
	if !strings.HasPrefix(s.hd.Get("Location"), "/v2/all/corp/app/blobs/uploads/") {
		t.Fatalf("the Location of a session: %q", s.hd.Get("Location"))
	}
	r.expect(r.do("PATCH", s.hd.Get("Location"), strings.NewReader("part")), 202)
	r.expect(r.do("PUT", s.hd.Get("Location")+"?digest=sha256:"+sum([]byte("part")), nil), 201)
	r.expect(r.do("PUT", "/v2/all/corp/app/manifests/v1", bytes.NewReader(ours.manifest), "Content-Type", "application/vnd.oci.image.manifest.v1+json"), 201)

	// "mine" holds it; the group answers with it, though the proxy comes first, and the upstream is not asked about corp/app.
	r.pull("all/corp/app", "v1", ours)
	for _, s := range f.seen {
		if strings.Contains(s, "corp/app") {
			t.Fatalf("dependency confusion: the upstream was asked %q", s)
		}
	}
	r.pull("mine/corp/app", "v1", ours)
	// A claimed prefix that nobody has published: not found, and not asked.
	r.expect(r.get("/v2/all/corp/secret/manifests/v1"), 404)
	r.expect(r.get("/v2/all/corp/secret/tags/list"), 404)
	for _, s := range f.seen {
		if strings.Contains(s, "corp/secret") {
			t.Fatalf("a claimed prefix was asked: %q", s)
		}
	}
	// A public name goes to the proxy.
	r.pull("all/library/alpine", "3", pub)
	// The tags of the group are the owner's.
	l := r.expect(r.get("/v2/all/corp/app/tags/list"), 200)
	if !bytes.Contains(l.body, []byte(`"v1"`)) || !bytes.Contains(l.body, []byte(`"all/corp/app"`)) {
		t.Fatalf("tags: %s", l.body)
	}
	// A group with no write_to takes no push.
	r.expect(r.do("POST", "/v2/ro/x/blobs/uploads/", nil), 405)
	// Not a name of anybody.
	r.expect(r.get("/v2/all/library/none/manifests/1"), 404)
	// Deleting the image does not give the name to the upstream: it was published here, so it is claimed.
	r.expect(r.do("DELETE", "/v2/all/corp/app/manifests/v1", nil), 202)
	r.expect(r.get("/v2/all/corp/app/manifests/v1"), 404)
	for _, s := range f.seen {
		if strings.Contains(s, "corp/app") {
			t.Fatalf("after the delete the upstream was asked %q", s)
		}
	}
}
