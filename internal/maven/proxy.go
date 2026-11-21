package maven

import (
	"bytes"
	"context"
	"encoding/hex"
	"hash"
	"net/http"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/raw"
)

const maxMetadata = 16 << 20

// capture takes an answer into memory (a checksum file, a cached metadata), up to maxMetadata.
type capture struct {
	h    http.Header
	code int
	buf  bytes.Buffer
	full bool
}

func (c *capture) Header() http.Header { return c.h }
func (c *capture) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}
func (c *capture) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	if c.buf.Len()+len(b) > maxMetadata {
		c.full = true
		return len(b), nil
	}
	return c.buf.Write(b)
}

// fetch gets a small file of a proxy through raw (the cache, or the upstream when the cache lacks it or its time is past; stale when the upstream cannot answer).
func (m *Maven) fetch(ctx context.Context, r *http.Request, q format.Request, path string) *capture {
	qq := q
	qq.Path = path
	qq.Authorize = func(http.ResponseWriter, string, string) bool { return true } // the request was authorized
	qq.Allowed = func(string, string) bool { return true }
	rr := r.Clone(ctx)
	rr.Method = http.MethodGet
	rr.Header.Del("Range")
	rr.Header.Del("If-None-Match")
	c := &capture{h: http.Header{}}
	m.Raw.Serve(c, rr, qq)
	return c
}

// proxySum answers a request for the checksum of a maven-metadata.xml of a proxy: it is made of the metadata that this proxy holds, so that a client that reads the two sees one pair (the checksum file of the
// upstream may be of a newer metadata than the copy that is kept).
func (m *Maven) proxySum(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Read, q.Path) {
		return
	}
	c := m.fetch(r.Context(), r, q, q.Path[:len(q.Path)-len(t.algo)-1])
	if c.code != http.StatusOK || c.full {
		code := c.code
		if code == 0 || c.full {
			code = http.StatusBadGateway
		}
		if v := c.h.Get("Retry-After"); v != "" {
			w.Header().Set("Retry-After", v)
		}
		fail(w, code, "the metadata is not available")
		return
	}
	h := newHash(t.algo)
	_, _ = h.Write(c.buf.Bytes())
	if v := c.h.Get("Warning"); v != "" {
		w.Header().Set("Warning", v)
	}
	write(w, r, []byte(hex.EncodeToString(h.Sum(nil))), "text/plain; charset=utf-8")
}

// source says where a proxied file comes from, and what the upstream names of it: a file of a version is checked against its checksum file (the strongest the upstream has: sha512, sha256, sha1); one
// whose upstream has no checksum file is not served (P1). Metadata and the checksum files are fetched as they are.
func (m *Maven) source(ctx context.Context, h *raw.Raw, q format.Request, path string) (*raw.Source, error) {
	t := parse(path)
	if t.kind != kFile {
		return &raw.Source{Path: path}, nil
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	for _, a := range []string{"sha512", "sha256", "sha1"} {
		c := m.fetch(ctx, r, q, path+"."+a)
		if c.code != http.StatusOK || c.full {
			continue
		}
		if s := firstHex(c.buf.Bytes(), sizeOf(a)); s != "" {
			sum, _ := hex.DecodeString(s)
			return &raw.Source{Path: path, Also: &raw.Hash{New: func() hash.Hash { return newHash(a) }, Sum: sum}}, nil
		}
	}
	return nil, raw.ErrNoSource
}
