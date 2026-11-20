package oci_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/oci"
	"github.com/zystem/godepot/internal/server"
	"github.com/zystem/godepot/internal/settings"
	"github.com/zystem/godepot/internal/testutil"
)

const settingsText = `{
 "repositories": [
  {"name": "img", "format": "oci", "kind": "hosted", "immutable_tags": ["v*"]},
  {"name": "pub", "format": "oci", "kind": "hosted", "anonymous_read": true}
 ],
 "grants": [
  {"account": "ci", "repositories": ["img"], "paths": ["team/**"], "actions": ["read", "write"]}
 ]
}`

type rig struct {
	t     *testing.T
	ts    *httptest.Server
	db    meta.DB
	layer *blob.Layer
	oci   *oci.OCI
	clock *atomic.Int64
	admin string
	ci    string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db := testutil.DB(t)
	clock := &atomic.Int64{}
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	layer := &blob.Layer{DB: db, Store: testutil.NewMemStore(), Now: now, Grace: time.Hour, UploadTTL: time.Hour}
	al := &audit.Log{DB: db, Now: now}
	as := &auth.Store{DB: db, Now: now, MaxLife: 200 * time.Hour}
	set, err := settings.Parse(strings.NewReader(settingsText))
	if err != nil {
		t.Fatal(err)
	}
	o := &oci.OCI{Blobs: layer, DB: db, Audit: al, Now: now, UploadTTL: time.Hour, MaxManifest: 1 << 20}
	s := &server.Server{Auth: as, Audit: al, Formats: map[string]format.Format{"oci": o}}
	s.Settings.Store(set)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	ctx := context.Background()
	for _, a := range []struct {
		n     string
		admin bool
	}{{"root", true}, {"ci", false}} {
		if err := as.CreateAccount(ctx, a.n, "user", a.admin); err != nil {
			t.Fatal(err)
		}
	}
	all := auth.Grant{Repositories: []string{"*"}, Actions: []string{auth.Read, auth.Write, auth.Delete, auth.Promote, auth.Admin}}
	adm, _, err := as.CreateToken(ctx, "root", []auth.Grant{all}, 100*time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	ci, _, err := as.CreateToken(ctx, "ci", []auth.Grant{all}, 100*time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, ts: ts, db: db, layer: layer, oci: o, clock: clock, admin: adm, ci: ci}
}

type reply struct {
	code int
	hd   http.Header
	body []byte
}

func (r *rig) do(method, path, token string, body io.Reader, headers ...string) reply {
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
	return reply{resp.StatusCode, resp.Header, b}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// errCode reads the first code of the "errors" list of an answer.
func errCode(t *testing.T, a reply) string {
	t.Helper()
	var e struct {
		Errors []struct{ Code string }
	}
	if err := json.Unmarshal(a.body, &e); err != nil || len(e.Errors) == 0 {
		t.Fatalf("no errors list in %d %s", a.code, a.body)
	}
	return e.Errors[0].Code
}

func want(t *testing.T, a reply, code int) reply {
	t.Helper()
	if a.code != code {
		t.Fatalf("status %d, want %d: %s", a.code, code, a.body)
	}
	return a
}

// pushBlob pushes bytes by the two requests of a client that sends one body (POST, then PUT).
func (r *rig) pushBlob(name string, data []byte) string {
	r.t.Helper()
	a := want(r.t, r.do("POST", "/v2/"+name+"/blobs/uploads/", r.admin, nil), 202)
	loc := a.hd.Get("Location")
	want(r.t, r.do("PUT", loc+"?digest="+sum(data), r.admin, bytes.NewReader(data)), 201)
	return sum(data)
}

func (r *rig) pushManifest(name, ref string, m map[string]any, mt string) (reply, string) {
	r.t.Helper()
	b, _ := json.Marshal(m)
	return r.do("PUT", "/v2/"+name+"/manifests/"+ref, r.admin, bytes.NewReader(b), "Content-Type", mt), sum(b)
}

func image(config, layer string) map[string]any {
	return map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": config, "size": 2},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": layer, "size": 5}}}
}

const mtImage = "application/vnd.oci.image.manifest.v1+json"

// A client pushes an image and pulls it back: the check of the door, a blob in one body and in parts, the manifest by tag, the lists.
func TestPushAndPull(t *testing.T) {
	r := newRig(t)
	if a := r.do("GET", "/v2/", "", nil); a.code != 401 || a.hd.Get("Www-Authenticate") == "" || errCode(t, a) != "UNAUTHORIZED" {
		t.Fatalf("the check without a token: %d %v %s", a.code, a.hd, a.body)
	}
	want(t, r.do("GET", "/v2/", r.admin, nil), 200)

	cfg := r.pushBlob("img/app", []byte("{}"))
	// A layer in three parts: the digest is of all of them.
	layer := []byte("hello world, in parts")
	a := want(t, r.do("POST", "/v2/img/app/blobs/uploads/", r.admin, nil), 202)
	loc := a.hd.Get("Location")
	a = want(t, r.do("PATCH", loc, r.admin, bytes.NewReader(layer[:5]), "Content-Range", "0-4"), 202)
	if a.hd.Get("Range") != "0-4" {
		t.Fatalf("Range %q", a.hd.Get("Range"))
	}
	a = want(t, r.do("PATCH", loc, r.admin, bytes.NewReader(layer[5:11])), 202)
	if a.hd.Get("Range") != "0-10" {
		t.Fatalf("Range %q", a.hd.Get("Range"))
	}
	if st := want(t, r.do("GET", loc, r.admin, nil), 204); st.hd.Get("Range") != "0-10" {
		t.Fatalf("status of the upload: %v", st.hd)
	}
	// A part that does not begin where the upload ends is refused.
	want(t, r.do("PATCH", loc, r.admin, bytes.NewReader(layer[11:]), "Content-Range", "3-20"), 416)
	want(t, r.do("PUT", loc+"?digest="+sum(layer), r.admin, bytes.NewReader(layer[11:])), 201)
	ld := sum(layer)
	b := want(t, r.do("GET", "/v2/img/app/blobs/"+ld, r.admin, nil), 200)
	if !bytes.Equal(b.body, layer) || b.hd.Get("Docker-Content-Digest") != ld {
		t.Fatalf("the layer read back: %q %v", b.body, b.hd)
	}
	if rg := want(t, r.do("GET", "/v2/img/app/blobs/"+ld, r.admin, nil, "Range", "bytes=6-10"), 206); string(rg.body) != "world" {
		t.Fatalf("range: %q", rg.body)
	}
	want(t, r.do("HEAD", "/v2/img/app/blobs/"+ld, r.admin, nil), 200)
	want(t, r.do("GET", "/v2/img/app/blobs/uploads/"+strings.Repeat("0", 32), r.admin, nil), 404)

	// The manifest, by tag.
	pa, md := r.pushManifest("img/app", "latest", image(cfg, ld), mtImage)
	want(t, pa, 201)
	if pa.hd.Get("Docker-Content-Digest") != md {
		t.Fatalf("digest %s != %s", pa.hd.Get("Docker-Content-Digest"), md)
	}
	g := want(t, r.do("GET", "/v2/img/app/manifests/latest", r.admin, nil), 200)
	if g.hd.Get("Content-Type") != mtImage || sum(g.body) != md {
		t.Fatalf("manifest read back: %v", g.hd)
	}
	want(t, r.do("GET", "/v2/img/app/manifests/"+md, r.admin, nil), 200)
	want(t, r.do("HEAD", "/v2/img/app/manifests/latest", r.admin, nil), 200)
	want(t, r.do("GET", "/v2/img/app/manifests/nope", r.admin, nil), 404)

	tl := want(t, r.do("GET", "/v2/img/app/tags/list", r.admin, nil), 200)
	if !strings.Contains(string(tl.body), `"tags":["latest"]`) {
		t.Fatalf("tags: %s", tl.body)
	}
	// Another name has not got the blobs.
	want(t, r.do("GET", "/v2/img/other/blobs/"+ld, r.admin, nil), 404)
	want(t, r.do("GET", "/v2/img/other/tags/list", r.admin, nil), 404)
}

// What goes wrong at the door is told in the words of the API.
func TestRefusals(t *testing.T) {
	r := newRig(t)
	data := []byte("some bytes")
	// The bytes are not the digest that was named: nothing is kept.
	a := want(t, r.do("POST", "/v2/img/app/blobs/uploads/?digest="+sum([]byte("other")), r.admin, bytes.NewReader(data)), 400)
	if errCode(t, a) != "DIGEST_INVALID" {
		t.Fatal(string(a.body))
	}
	want(t, r.do("GET", "/v2/img/app/blobs/"+sum(data), r.admin, nil), 404)
	// A manifest that names what the name does not have.
	missing := sum([]byte("no such"))
	pa, _ := r.pushManifest("img/app", "x", image(missing, missing), mtImage)
	if want(t, pa, 400); errCode(t, pa) != "MANIFEST_BLOB_UNKNOWN" {
		t.Fatal(string(pa.body))
	}
	cfg := r.pushBlob("img/app", []byte("{}"))
	l := r.pushBlob("img/app", data)
	// By digest, with other bytes.
	b, _ := json.Marshal(image(cfg, l))
	a = want(t, r.do("PUT", "/v2/img/app/manifests/"+sum([]byte("x")), r.admin, bytes.NewReader(b), "Content-Type", mtImage), 400)
	if errCode(t, a) != "DIGEST_INVALID" {
		t.Fatal(string(a.body))
	}
	// The media type must be one that is known, and the same as the body says.
	want(t, r.do("PUT", "/v2/img/app/manifests/x", r.admin, bytes.NewReader(b), "Content-Type", "text/plain"), 400)
	want(t, r.do("PUT", "/v2/img/app/manifests/x", r.admin, bytes.NewReader(b), "Content-Type", "application/vnd.oci.image.index.v1+json"), 400)
	// Names and tags.
	want(t, r.do("GET", "/v2/img/Bad_Name/tags/list", r.admin, nil), 400)
	want(t, r.do("GET", "/v2/img/tags/list", r.admin, nil), 400) // no image
	want(t, r.do("GET", "/v2/nosuch/app/tags/list", r.admin, nil), 404)
	want(t, r.do("PUT", "/v2/img/app/manifests/.bad", r.admin, bytes.NewReader(b), "Content-Type", mtImage), 400)
	// A blob is not deleted by a client.
	want(t, r.do("DELETE", "/v2/img/app/blobs/"+l, r.admin, nil), 405)
}

// A tag that the repository calls immutable never moves; the others do.
func TestImmutableTags(t *testing.T) {
	r := newRig(t)
	cfg := r.pushBlob("img/app", []byte("{}"))
	l1, l2 := r.pushBlob("img/app", []byte("one")), r.pushBlob("img/app", []byte("two"))
	for _, tag := range []string{"v1", "dev"} {
		pa, _ := r.pushManifest("img/app", tag, image(cfg, l1), mtImage)
		want(t, pa, 201)
	}
	pa, _ := r.pushManifest("img/app", "v1", image(cfg, l1), mtImage) // the same bytes again
	want(t, pa, 201)
	pa, _ = r.pushManifest("img/app", "v1", image(cfg, l2), mtImage)
	if want(t, pa, 409); errCode(t, pa) != "DENIED" {
		t.Fatal(string(pa.body))
	}
	pa, md := r.pushManifest("img/app", "dev", image(cfg, l2), mtImage)
	want(t, pa, 201)
	g := want(t, r.do("GET", "/v2/img/app/manifests/dev", r.admin, nil), 200)
	if sum(g.body) != md {
		t.Fatal("the tag did not move")
	}
}

// An index, its children, and the rule that a manifest an index names is not deleted under it (docs/spec.md, C5).
func TestIndexAndDelete(t *testing.T) {
	r := newRig(t)
	cfg := r.pushBlob("img/app", []byte("{}"))
	l := r.pushBlob("img/app", []byte("layer"))
	pa, child := r.pushManifest("img/app", "child", image(cfg, l), mtImage)
	want(t, pa, 201)
	idx := map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{"mediaType": mtImage, "digest": child, "size": 100}}}
	pa, _ = r.pushManifest("img/app", "multi", idx, "application/vnd.oci.image.index.v1+json")
	want(t, pa, 201)
	// An index that names a manifest the name lacks.
	bad := map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{"mediaType": mtImage, "digest": sum([]byte("zz")), "size": 1}}}
	pa, _ = r.pushManifest("img/app", "bad", bad, "application/vnd.oci.image.index.v1+json")
	want(t, pa, 400)

	a := want(t, r.do("DELETE", "/v2/img/app/manifests/"+child, r.admin, nil), 409)
	if errCode(t, a) != "DENIED" {
		t.Fatal(string(a.body))
	}
	// Removing a tag removes the tag and nothing else.
	want(t, r.do("DELETE", "/v2/img/app/manifests/multi", r.admin, nil), 202)
	want(t, r.do("GET", "/v2/img/app/manifests/"+child, r.admin, nil), 200)
	want(t, r.do("DELETE", "/v2/img/app/manifests/multi", r.admin, nil), 404)
	// The index itself is still there by digest, and with it the rule.
	want(t, r.do("DELETE", "/v2/img/app/manifests/"+child, r.admin, nil), 409)
	_, idxDigest := r.pushManifest("img/app", "multi", idx, "application/vnd.oci.image.index.v1+json")
	want(t, r.do("DELETE", "/v2/img/app/manifests/"+idxDigest, r.admin, nil), 202)
	want(t, r.do("DELETE", "/v2/img/app/manifests/"+child, r.admin, nil), 202)
	// Its tag went with it.
	want(t, r.do("GET", "/v2/img/app/manifests/child", r.admin, nil), 404)
}

// A signature or any artifact names a subject; the API lists them, with the filter.
func TestReferrers(t *testing.T) {
	r := newRig(t)
	cfg := r.pushBlob("img/app", []byte("{}"))
	l := r.pushBlob("img/app", []byte("layer"))
	pa, subject := r.pushManifest("img/app", "latest", image(cfg, l), mtImage)
	want(t, pa, 201)
	sig := image(cfg, l)
	sig["artifactType"] = "application/vnd.example.sig"
	sig["subject"] = map[string]any{"mediaType": mtImage, "digest": subject, "size": 10}
	sig["annotations"] = map[string]string{"org.example": "yes"}
	pa, sd := r.pushManifest("img/app", sd0(sig), sig, mtImage)
	want(t, pa, 201)
	if pa.hd.Get("OCI-Subject") != subject {
		t.Fatalf("OCI-Subject %q", pa.hd.Get("OCI-Subject"))
	}
	a := want(t, r.do("GET", "/v2/img/app/referrers/"+subject, r.admin, nil), 200)
	var idx struct {
		Manifests []struct {
			Digest, ArtifactType string
			Annotations          map[string]string
		}
	}
	if err := json.Unmarshal(a.body, &idx); err != nil || len(idx.Manifests) != 1 || idx.Manifests[0].Digest != sd || idx.Manifests[0].Annotations["org.example"] != "yes" {
		t.Fatalf("referrers: %s", a.body)
	}
	a = want(t, r.do("GET", "/v2/img/app/referrers/"+subject+"?artifactType=other", r.admin, nil), 200)
	if a.hd.Get("OCI-Filters-Applied") != "artifactType" || strings.Contains(string(a.body), sd) {
		t.Fatalf("filtered: %v %s", a.hd, a.body)
	}
	// Deleting the subject does not delete the signature.
	want(t, r.do("DELETE", "/v2/img/app/manifests/latest", r.admin, nil), 202)
	want(t, r.do("DELETE", "/v2/img/app/manifests/"+subject, r.admin, nil), 202)
	a = want(t, r.do("GET", "/v2/img/app/referrers/"+subject, r.admin, nil), 200)
	if !strings.Contains(string(a.body), sd) {
		t.Fatalf("the signature went with its subject: %s", a.body)
	}
}

// sd0 makes a distinct tag for a manifest (the test pushes it by tag).
func sd0(m map[string]any) string { b, _ := json.Marshal(m); return "sig" + sum(b)[7:15] }

// A blob of one name is mounted into another of the same repository; a blob that is not there is not.
func TestMount(t *testing.T) {
	r := newRig(t)
	d := r.pushBlob("img/a", []byte("shared layer"))
	want(t, r.do("GET", "/v2/img/b/blobs/"+d, r.admin, nil), 404)
	a := want(t, r.do("POST", "/v2/img/b/blobs/uploads/?mount="+d+"&from=img/a", r.admin, nil), 201)
	if a.hd.Get("Docker-Content-Digest") != d {
		t.Fatal("no digest in the answer")
	}
	want(t, r.do("GET", "/v2/img/b/blobs/"+d, r.admin, nil), 200)
	// Not there: an upload is started instead.
	a = want(t, r.do("POST", "/v2/img/c/blobs/uploads/?mount="+sum([]byte("zz"))+"&from=img/a", r.admin, nil), 202)
	if a.hd.Get("Location") == "" {
		t.Fatal("no Location")
	}
	// Another repository is not mounted from.
	want(t, r.do("POST", "/v2/img/c/blobs/uploads/?mount="+d+"&from=pub/a", r.admin, nil), 202)
}

// Who may do what: the paths of the grants are images; the answers are those of the API.
func TestAccess(t *testing.T) {
	r := newRig(t)
	// ci has a token as wide as it can be made, and the grants of its account: write in team/** of img.
	d := []byte("x")
	a := r.do("POST", "/v2/img/team/app/blobs/uploads/?digest="+sum(d), r.ci, bytes.NewReader(d))
	want(t, a, 201)
	a = r.do("POST", "/v2/img/other/app/blobs/uploads/?digest="+sum(d), r.ci, bytes.NewReader(d))
	if want(t, a, 403); errCode(t, a) != "DENIED" {
		t.Fatal(string(a.body))
	}
	want(t, r.do("DELETE", "/v2/img/team/app/manifests/latest", r.ci, nil), 403) // no delete action
	want(t, r.do("GET", "/v2/img/team/app/tags/list", "", nil), 401)
	// A repository that lets anyone read.
	cfg := r.pushBlob("pub/app", []byte("{}"))
	l := r.pushBlob("pub/app", []byte("l"))
	pa, _ := r.pushManifest("pub/app", "latest", image(cfg, l), mtImage)
	want(t, pa, 201)
	want(t, r.do("GET", "/v2/pub/app/manifests/latest", "", nil), 200)
	want(t, r.do("GET", "/v2/pub/app/blobs/"+l, "", nil), 200)
	want(t, r.do("POST", "/v2/pub/app/blobs/uploads/", "", nil), 401)
	// A bad token.
	want(t, r.do("GET", "/v2/", "gd_aaaaaaaaaaaa_"+strings.Repeat("b", 43), nil), 401)
}

// The tags are listed in order and a page at a time.
func TestTagPages(t *testing.T) {
	r := newRig(t)
	cfg := r.pushBlob("img/app", []byte("{}"))
	l := r.pushBlob("img/app", []byte("l"))
	for _, tag := range []string{"c", "a", "d", "b"} {
		pa, _ := r.pushManifest("img/app", tag, image(cfg, l), mtImage)
		want(t, pa, 201)
	}
	get := func(q string) (tags []string, link string) {
		a := want(t, r.do("GET", "/v2/img/app/tags/list"+q, r.admin, nil), 200)
		var v struct{ Tags []string }
		if err := json.Unmarshal(a.body, &v); err != nil {
			t.Fatal(err)
		}
		return v.Tags, a.hd.Get("Link")
	}
	if tags, link := get(""); fmt.Sprint(tags) != "[a b c d]" || link != "" {
		t.Fatalf("all: %v %q", tags, link)
	}
	tags, link := get("?n=2")
	if fmt.Sprint(tags) != "[a b]" || !strings.Contains(link, "last=b") || !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1: %v %q", tags, link)
	}
	if tags, link := get("?n=2&last=b"); fmt.Sprint(tags) != "[c d]" || link != "" {
		t.Fatalf("page 2: %v %q", tags, link)
	}
	if tags, _ := get("?n=0"); len(tags) != 0 {
		t.Fatalf("n=0: %v", tags)
	}
	want(t, r.do("GET", "/v2/img/app/tags/list?n=x", r.admin, nil), 400)
}

// A manifest larger than the row holds is kept as a blob and reads back whole.
func TestLargeManifest(t *testing.T) {
	r := newRig(t)
	cfg := r.pushBlob("img/app", []byte("{}"))
	l := r.pushBlob("img/app", []byte("l"))
	m := image(cfg, l)
	m["annotations"] = map[string]string{"big": strings.Repeat("x", 300<<10)}
	pa, md := r.pushManifest("img/app", "big", m, mtImage)
	want(t, pa, 201)
	g := want(t, r.do("GET", "/v2/img/app/manifests/big", r.admin, nil), 200)
	if sum(g.body) != md || len(g.body) < 300<<10 {
		t.Fatalf("the large manifest read back wrong: %d bytes", len(g.body))
	}
	want(t, r.do("DELETE", "/v2/img/app/manifests/"+md, r.admin, nil), 202)
	// Over the limit of the rig (1 MiB).
	m["annotations"] = map[string]string{"big": strings.Repeat("x", 2<<20)}
	pa, _ = r.pushManifest("img/app", "huge", m, mtImage)
	want(t, pa, 413)
}

// An upload that nobody ends is swept, and its parts are let go.
func TestSweepOfUploads(t *testing.T) {
	r := newRig(t)
	a := want(t, r.do("POST", "/v2/img/app/blobs/uploads/", r.admin, nil), 202)
	loc := a.hd.Get("Location")
	want(t, r.do("PATCH", loc, r.admin, strings.NewReader("part one")), 202)
	ctx := context.Background()
	if n, err := r.oci.Sweep(ctx, 10); err != nil || n != 0 {
		t.Fatalf("swept %d before its time: %v", n, err)
	}
	r.clock.Add(2 * 3600)
	if n, err := r.oci.Sweep(ctx, 10); err != nil || n != 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	want(t, r.do("GET", loc, r.admin, nil), 404)
	// The part is now a blob that nothing names: the collector removes it after the grace time.
	r.clock.Add(2 * 3600)
	got, err := r.layer.Collect(ctx, 100)
	if err != nil || got.Blobs != 1 {
		t.Fatalf("collected %+v, %v", got, err)
	}
}

// The audit log has a line for every write and delete of the API.
func TestAudit(t *testing.T) {
	r := newRig(t)
	cfg := r.pushBlob("img/app", []byte("{}"))
	l := r.pushBlob("img/app", []byte("l"))
	pa, md := r.pushManifest("img/app", "latest", image(cfg, l), mtImage)
	want(t, pa, 201)
	want(t, r.do("DELETE", "/v2/img/app/manifests/"+md, r.admin, nil), 202)
	rows, err := r.db.Query(context.Background(), meta.S(`SELECT action, path, result FROM audit ORDER BY id`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, meta.Str(row[0])+" "+meta.Str(row[1])+" "+meta.Str(row[2]))
	}
	text := strings.Join(got, "\n")
	if len(got) != 4 || !strings.Contains(text, "put app/manifests/latest ok") || !strings.Contains(text, "delete app/manifests/"+md+" ok") {
		t.Fatalf("audit rows:\n%s", text)
	}
}

// Downloads at once are limited: the one over the limit waits a little and is told to come back (docs/spec.md, P11).
func TestDownloadLimit(t *testing.T) {
	r := newRig(t)
	d := r.pushBlob("img/app", []byte("a blob that is read"))
	r.oci.Downloads = format.NewGate(1, 50*time.Millisecond)
	release, ok := r.oci.Downloads.Acquire(context.Background())
	if !ok {
		t.Fatal("no place")
	}
	a := want(t, r.do("GET", "/v2/img/app/blobs/"+d, r.admin, nil), 503)
	if a.hd.Get("Retry-After") == "" || errCode(t, a) != "UNAVAILABLE" {
		t.Fatalf("the answer over the limit: %v %s", a.hd, a.body)
	}
	// What is not a download of bytes is not limited.
	want(t, r.do("HEAD", "/v2/img/app/blobs/"+d, r.admin, nil), 200)
	release()
	want(t, r.do("GET", "/v2/img/app/blobs/"+d, r.admin, nil), 200)
	if r.oci.Downloads.InUse() != 0 {
		t.Fatalf("a place is still taken: %d", r.oci.Downloads.InUse())
	}
}

// Writers of the same blob and the same tag at once all succeed and leave one row and one reference; parts sent at once are told apart; two ends of one upload make one blob.
func TestRaces(t *testing.T) {
	r := newRig(t)
	layer := []byte("the layer that everybody pushes")
	d := sum(layer)
	cfg := r.pushBlob("img/app", []byte("{}"))
	var wg sync.WaitGroup
	codes := make(chan int, 64)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := r.do("POST", "/v2/img/app/blobs/uploads/?digest="+d, r.admin, bytes.NewReader(layer))
			codes <- a.code
			b, _ := json.Marshal(image(cfg, d))
			codes <- r.do("PUT", "/v2/img/app/manifests/latest", r.admin, bytes.NewReader(b), "Content-Type", mtImage).code
		}()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 201 {
			t.Fatalf("a writer got %d", c)
		}
	}
	ctx := context.Background()
	one := func(sql string, args ...any) int64 {
		rows, err := r.db.Query(ctx, meta.S(sql, args...))
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: %v %v", sql, rows, err)
		}
		return meta.Int(rows[0][0])
	}
	if n := one(`SELECT COUNT(*) FROM oci_blobs WHERE digest = ?`, d[7:]); n != 1 {
		t.Fatalf("%d rows for one blob", n)
	}
	if n := one(`SELECT COUNT(*) FROM refs WHERE digest = ?`, d[7:]); n != 1 {
		t.Fatalf("%d references to the layer, want 1 (the name's)", n)
	}
	if n := one(`SELECT COUNT(*) FROM oci_manifests`); n != 1 {
		t.Fatalf("%d manifests", n)
	}

	// Parts at once: each is taken or told to look again; the size is that of the parts taken.
	a := want(t, r.do("POST", "/v2/img/app/blobs/uploads/", r.admin, nil), 202)
	loc := a.hd.Get("Location")
	payload := map[string]string{}
	var mu sync.Mutex
	var ok atomic.Int64
	for i := 0; i < 8; i++ {
		p := fmt.Sprintf("part-%02d-----", i) // 12 bytes each
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch c := r.do("PATCH", loc, r.admin, strings.NewReader(p)).code; c {
			case 202:
				ok.Add(1)
				mu.Lock()
				payload[sum([]byte(p))[7:]] = p
				mu.Unlock()
			case 416:
			default:
				t.Errorf("PATCH answered %d", c)
			}
		}()
	}
	wg.Wait()
	if ok.Load() == 0 {
		t.Fatal("no part was taken")
	}
	st := want(t, r.do("GET", loc, r.admin, nil), 204)
	if st.hd.Get("Range") != fmt.Sprintf("0-%d", ok.Load()*12-1) {
		t.Fatalf("the upload has %s after %d parts of 12 bytes", st.hd.Get("Range"), ok.Load())
	}
	rows, err := r.db.Query(ctx, meta.S(`SELECT digest FROM oci_parts ORDER BY n`))
	if err != nil {
		t.Fatal(err)
	}
	var whole string
	for _, row := range rows {
		whole += payload[meta.Str(row[0])]
	}
	// A digest that is not the bytes: refused, and the upload stays.
	b := want(t, r.do("PUT", loc+"?digest="+sum([]byte("not these")), r.admin, nil), 400)
	if errCode(t, b) != "DIGEST_INVALID" {
		t.Fatal(string(b.body))
	}
	want(t, r.do("GET", loc, r.admin, nil), 204)
	// Two ends of one upload at once make one blob; the other is told the upload is gone, or ends it as well.
	var ends [2]int
	for i := range ends {
		wg.Add(1)
		go func() { defer wg.Done(); ends[i] = r.do("PUT", loc+"?digest="+sum([]byte(whole)), r.admin, nil).code }()
	}
	wg.Wait()
	if !(ends[0] == 201 || ends[1] == 201) || ends[0] != 201 && ends[0] != 404 || ends[1] != 201 && ends[1] != 404 {
		t.Fatalf("two ends of one upload: %v", ends)
	}
	got := want(t, r.do("GET", "/v2/img/app/blobs/"+sum([]byte(whole)), r.admin, nil), 200)
	if string(got.body) != whole {
		t.Fatalf("the blob is %q, want %q", got.body, whole)
	}
	if n := one(`SELECT COUNT(*) FROM oci_uploads`); n != 0 {
		t.Fatalf("%d uploads left", n)
	}
}

// Every way that a request can be wrong is told in the words of the API, and the answers a client relies on have the headers it needs.
func TestMoreRefusals(t *testing.T) {
	r := newRig(t)
	d := r.pushBlob("img/app", []byte("0123456789"))
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{"POST", "/v2/img/app/manifests/latest", 405},
		{"PUT", "/v2/img/app/blobs/" + d, 405},
		{"PATCH", "/v2/img/app/tags/list", 405},
		{"POST", "/v2/img/app/referrers/" + d, 405},
		{"GET", "/v2/img/app/blobs/sha256:xyz", 400},
		{"GET", "/v2/img/app/referrers/sha256:xyz", 400},
		{"GET", "/v2/img/app/blobs/" + sum([]byte("no")), 404},
		{"GET", "/v2/img/app/nonsense", 404},
		{"GET", "/v2/img/app/manifests/sha256:bad", 404},
		{"PUT", "/v2/img/app/manifests/sha256:bad", 400},
		{"PUT", "/v2/img/app/blobs/uploads/" + strings.Repeat("0", 32) + "?digest=" + d, 404},
		{"DELETE", "/v2/img/app/blobs/uploads/nonsense", 404},
		{"POST", "/v2/img/app/blobs/uploads/?digest=sha256:bad", 400},
	} {
		a := r.do(c.method, c.path, r.admin, strings.NewReader("{}"))
		if a.code != c.code {
			t.Errorf("%s %s: %d, want %d: %s", c.method, c.path, a.code, c.code, a.body)
		}
	}
	if a := want(t, r.do("GET", "/v2/img/app/blobs/"+d, r.admin, nil, "Range", "bytes=50-60"), 416); a.hd.Get("Content-Range") != "bytes */10" {
		t.Fatalf("Content-Range %q", a.hd.Get("Content-Range"))
	}
	// A manifest that is not JSON, and one with a schema that is not 2, and one with no config.
	for _, body := range []string{"not json", `{"schemaVersion": 1, "mediaType": "` + mtImage + `"}`, `{"schemaVersion": 2, "mediaType": "` + mtImage + `"}`,
		`{"schemaVersion": 2, "mediaType": "` + mtImage + `", "config": {"digest": "sha256:zz"}}`} {
		want(t, r.do("PUT", "/v2/img/app/manifests/x", r.admin, strings.NewReader(body), "Content-Type", mtImage), 400)
	}
	// A subject that is not a digest.
	cfg := r.pushBlob("img/app", []byte("{}"))
	m := image(cfg, d)
	m["subject"] = map[string]any{"digest": "nope"}
	pa, _ := r.pushManifest("img/app", "s", m, mtImage)
	want(t, pa, 400)
	// A layer that is kept elsewhere is not looked for.
	f := image(cfg, sum([]byte("elsewhere")))
	f["layers"] = []any{map[string]any{"mediaType": "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip", "digest": sum([]byte("elsewhere")), "size": 3}}
	f["mediaType"] = "application/vnd.docker.distribution.manifest.v2+json"
	pa, _ = r.pushManifest("img/app", "foreign", f, "application/vnd.docker.distribution.manifest.v2+json")
	want(t, pa, 201)
	// An upload of another name is not found under this one.
	a := want(t, r.do("POST", "/v2/img/app/blobs/uploads/", r.admin, nil), 202)
	other := strings.Replace(a.hd.Get("Location"), "/img/app/", "/img/other/", 1)
	want(t, r.do("GET", other, r.admin, nil), 404)
	want(t, r.do("DELETE", a.hd.Get("Location"), r.admin, nil), 204)
	// An empty blob, in one request and as an upload with no part.
	empty := sum(nil)
	want(t, r.do("POST", "/v2/img/app/blobs/uploads/?digest="+empty, r.admin, nil), 201)
	a = want(t, r.do("POST", "/v2/img/app/blobs/uploads/", r.admin, nil), 202)
	want(t, r.do("PUT", a.hd.Get("Location")+"?digest="+empty, r.admin, nil), 201)
	if g := want(t, r.do("GET", "/v2/img/app/blobs/"+empty, r.admin, nil), 200); len(g.body) != 0 {
		t.Fatal("the empty blob is not empty")
	}
	// A part that has no bytes is taken and changes nothing.
	a = want(t, r.do("POST", "/v2/img/app/blobs/uploads/", r.admin, nil), 202)
	want(t, r.do("PATCH", a.hd.Get("Location"), r.admin, bytes.NewReader(nil), "Content-Length", "0"), 202)
}
