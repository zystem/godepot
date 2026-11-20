package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/testutil"
)

const ociSettings = `{"repositories": [{"name": "reg", "format": "oci", "kind": "hosted", "immutable_tags": ["v*"]}], "grants": []}`

// The whole program with a real rqlite and a real blobgate, spoken to as an OCI client does: a check of the door with a token, a layer of more than one block of blobgate sent in parts, a config and
// a manifest, and the pull back by tag and by digest, in the words of the specification.
func TestOCIThroughTheProgram(t *testing.T) {
	standEnv(t)
	ctx := context.Background()
	if err := testutil.Reset(ctx, mustDB(t)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "godepot.json")
	if err := os.WriteFile(file, []byte(ociSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODEPOT_CONFIG_FILE", file)
	if code, out, errs := runCmd("new-account", "root", "-admin"); code != 0 {
		t.Fatalf("new-account: %d %q %q", code, out, errs)
	}
	code, out, errs := runCmd("new-token", "root", "-life", "2h", "-repositories", "*", "-actions", "read,write,delete")
	tok := strings.TrimSpace(out)
	if code != 0 {
		t.Fatalf("new-token: %d %q", code, errs)
	}
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port, cfg.MetricsPort, cfg.CollectEvery = 18172, 18173, time.Second
	a := newApp(cfg, os.Stdout, &bytes.Buffer{})
	sctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.serve(sctx) }()
	base := "http://127.0.0.1:18172"
	call := func(method, path, token string, body io.Reader, hdr ...string) (int, http.Header, []byte) {
		t.Helper()
		var resp *http.Response
		for i := 0; i < 50; i++ {
			req, _ := http.NewRequest(method, base+path, body)
			if token != "" {
				req.SetBasicAuth("x", token)
			}
			for j := 0; j+1 < len(hdr); j += 2 {
				req.Header.Set(hdr[j], hdr[j+1])
			}
			if resp, err = http.DefaultClient.Do(req); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, b
	}
	sum := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

	if c, h, _ := call("GET", "/v2/", "", nil); c != 401 || h.Get("Www-Authenticate") == "" {
		t.Fatalf("the check without a token: %d %v", c, h)
	}
	if c, _, b := call("GET", "/v2/", tok, nil); c != 200 {
		t.Fatalf("the check with a token: %d %s", c, b)
	}
	layer := make([]byte, 20<<20+321) // more than two blocks of blobgate
	_, _ = rand.Read(layer)
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	push := func(data []byte, parts int) string {
		t.Helper()
		c, h, b := call("POST", "/v2/reg/app/blobs/uploads/", tok, nil)
		if c != 202 {
			t.Fatalf("start: %d %s", c, b)
		}
		loc := h.Get("Location")
		step := (len(data) + parts - 1) / parts
		for off := 0; off < len(data); off += step {
			end := min(off+step, len(data))
			if c, _, b := call("PATCH", loc, tok, bytes.NewReader(data[off:end])); c != 202 {
				t.Fatalf("patch: %d %s", c, b)
			}
		}
		if c, _, b := call("PUT", loc+"?digest="+sum(data), tok, nil); c != 201 {
			t.Fatalf("put: %d %s", c, b)
		}
		return sum(data)
	}
	ld, cd := push(layer, 3), push(config, 1)
	m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(config)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": ld, "size": len(layer)}}})
	if c, _, b := call("PUT", "/v2/reg/app/manifests/v1", tok, bytes.NewReader(m), "Content-Type", "application/vnd.oci.image.manifest.v1+json"); c != 201 {
		t.Fatalf("manifest: %d %s", c, b)
	}
	if c, _, b := call("GET", "/v2/reg/app/blobs/"+ld, tok, nil); c != 200 || !bytes.Equal(b, layer) {
		t.Fatalf("layer: %d, %d bytes of %d", c, len(b), len(layer))
	}
	if c, _, b := call("GET", "/v2/reg/app/blobs/"+ld, tok, nil, "Range", "bytes=10000000-10000099"); c != 206 || !bytes.Equal(b, layer[10000000:10000100]) {
		t.Fatalf("range: %d", c)
	}
	if c, h, b := call("GET", "/v2/reg/app/manifests/v1", tok, nil); c != 200 || !bytes.Equal(b, m) || h.Get("Docker-Content-Digest") != sum(m) {
		t.Fatalf("manifest read: %d %v", c, h)
	}
	if c, _, _ := call("PUT", "/v2/reg/app/manifests/v1", tok, bytes.NewReader(bytes.Replace(m, []byte(`"mediaType"`), []byte(`"mediaType" `), 1)), "Content-Type", "application/vnd.oci.image.manifest.v1+json"); c != 409 {
		t.Fatalf("an immutable tag moved: %d", c)
	}
	if c, _, b := call("GET", "/v2/reg/app/tags/list", tok, nil); c != 200 || !strings.Contains(string(b), `"v1"`) {
		t.Fatalf("tags: %d %s", c, b)
	}
	if c, _, _ := call("GET", "/v2/reg/app/manifests/v1", "", nil); c != 401 {
		t.Fatalf("no token: %d", c)
	}
	mt := &bytes.Buffer{}
	a.reg.Render(mt)
	for _, want := range []string{`godepot_writes_total{repo="reg"}`, `godepot_downloads_active 0`, `godepot_refused_total{reason="conflict"} 1`} {
		if !strings.Contains(mt.String(), want) {
			t.Errorf("the metrics lack %s:\n%s", want, mt)
		}
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not end")
	}
}
