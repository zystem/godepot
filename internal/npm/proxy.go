package npm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"net/http"
	"strings"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/raw"
)

const maxIndex = 128 << 20 // the largest packument of an upstream that is read

// capture takes an answer into memory (the cached packument of a proxy), up to maxIndex.
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
	if c.buf.Len()+len(b) > maxIndex {
		c.full = true
		return len(b), nil
	}
	return c.buf.Write(b)
}

// upDoc is what is read of the packument of an upstream.
type upDoc struct {
	Name     string                     `json:"name"`
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
	Modified string                     `json:"modified"`
}

type upVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Tarball   string `json:"tarball"`
		Shasum    string `json:"shasum"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

// cached gets the packument of a package from the cache of a proxy repository, which asks the upstream when the copy is missing or past its time to live (and serves it stale when the upstream
// cannot answer): the answer of raw for the path -/index/<pkg>.json, taken into memory.
func (p *NPM) cached(ctx context.Context, r *http.Request, q format.Request, pkg string) (*upDoc, *capture, error) {
	qq := q
	qq.Path = "-/index/" + pkg + ".json"
	qq.Authorize = func(http.ResponseWriter, string, string) bool { return true } // the request was authorized for the package
	qq.Allowed = func(string, string) bool { return true }
	rr := r.Clone(ctx)
	rr.Method = http.MethodGet
	rr.Header.Del("Range")
	rr.Header.Del("If-None-Match")
	c := &capture{h: http.Header{}}
	p.Raw.Serve(c, rr, qq)
	if c.code != http.StatusOK || c.full {
		return nil, c, nil
	}
	var d upDoc
	if err := json.Unmarshal(c.buf.Bytes(), &d); err != nil || d.Versions == nil {
		return nil, nil, errors.New("the upstream's packument is not JSON of a package")
	}
	return &d, c, nil
}

// proxyDoc answers a request for the packument (or the tags) of a package of a proxy: the cached document of the upstream, with the address of each tarball made ours, so that the client asks this
// registry for the bytes and not the upstream. Only versions that the upstream's own manifest names as this package are shown.
func (p *NPM) proxyDoc(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Read, t.pkg) {
		return
	}
	d, c, err := p.cached(r.Context(), r, q, t.pkg)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if d == nil {
		if v := c.h.Get("Retry-After"); v != "" {
			w.Header().Set("Retry-After", v)
		}
		code := c.code
		if code == 0 || c.full {
			code = http.StatusBadGateway
		}
		fail(w, code, "the packument of this package is not available: "+strings.TrimSpace(c.buf.String()))
		return
	}
	extra := http.Header{}
	if v := c.h.Get("Warning"); v != "" {
		extra.Set("Warning", v) // the copy is stale: the upstream could not be asked
	}
	if t.kind == kTags {
		b, _ := json.Marshal(d.DistTags)
		send(w, r, b, false, extra)
		return
	}
	root := base(r, q)
	versions := map[string]json.RawMessage{}
	for v, raw := range d.Versions {
		var uv upVersion
		if json.Unmarshal(raw, &uv) != nil || uv.Name != t.pkg || uv.Version != v || !validVersion(v) {
			continue
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		var dist map[string]json.RawMessage
		_ = json.Unmarshal(m["dist"], &dist)
		u, _ := json.Marshal(root + t.pkg + "/-/" + tarball(t.pkg, v))
		dist["tarball"] = u
		m["dist"], _ = json.Marshal(dist)
		versions[v], _ = json.Marshal(m)
	}
	if len(versions) == 0 {
		fail(w, http.StatusNotFound, "no such package")
		return
	}
	tags := map[string]string{}
	for tag, v := range d.DistTags {
		if _, ok := versions[v]; ok {
			tags[tag] = v
		}
	}
	out := map[string]any{"name": t.pkg, "dist-tags": tags, "versions": versions}
	if d.Modified != "" {
		out["modified"] = d.Modified
	}
	b, _ := json.Marshal(out)
	send(w, r, b, wantsAbbrev(r), extra)
}

// source says where a proxied file comes from: the packument of a package is the upstream's own, asked for in the abbreviated form (what an install needs, and much smaller); a tarball is at the
// address that packument names, with the hash it names.
func (p *NPM) source(ctx context.Context, h *raw.Raw, q format.Request, path string) (*raw.Source, error) {
	t, ok := parse(path)
	if !ok {
		return nil, raw.ErrNoSource
	}
	switch t.kind {
	case kCached:
		return &raw.Source{Path: t.pkg, Header: http.Header{"Accept": {mimeAbbrev + ", " + mimeJSON + "; q=0.8"}}}, nil
	case kTarball:
	default:
		return nil, raw.ErrNoSource
	}
	v, _ := version(t.pkg, t.file)
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	d, c, err := p.cached(ctx, r, q, t.pkg)
	if err != nil {
		return nil, err
	}
	if d == nil {
		if c != nil && (c.code == http.StatusNotFound || c.code == 0) {
			return nil, raw.ErrNoSource
		}
		return nil, errors.New("the packument of the package is not available")
	}
	var uv upVersion
	if json.Unmarshal(d.Versions[v], &uv) != nil || uv.Name != t.pkg || uv.Version != v || uv.Dist.Tarball == "" {
		return nil, raw.ErrNoSource
	}
	s := &raw.Source{URL: uv.Dist.Tarball}
	if s.Also, err = named(uv.Dist.Integrity, uv.Dist.Shasum); err != nil {
		return nil, err
	}
	return s, nil
}

// named reads the hash that the upstream names of a tarball: the strongest of its integrity (an SRI string), else its shasum. An upstream that names none is refused: nothing says what the bytes
// are, and they would be kept for ever as if they had been checked.
func named(integrity, shasum string) (*raw.Hash, error) {
	var best *raw.Hash
	rank := 0
	for _, tok := range strings.Fields(integrity) {
		algo, b64, ok := strings.Cut(tok, "-")
		if !ok {
			continue
		}
		b64, _, _ = strings.Cut(b64, "?")
		sum, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		var newH func() hash.Hash
		r := 0
		switch algo {
		case "sha512":
			newH, r = sha512.New, 3
		case "sha256":
			newH, r = sha256.New, 2
		}
		if newH != nil && len(sum) == newH().Size() && r > rank {
			best, rank = &raw.Hash{New: newH, Sum: sum}, r
		}
	}
	if best != nil {
		return best, nil
	}
	if sum, err := hex.DecodeString(shasum); err == nil && len(sum) == sha1Size {
		return &raw.Hash{New: sha1New, Sum: sum}, nil // the only hash this upstream names
	}
	return nil, errors.New("the upstream names no hash of this tarball (dist.integrity or dist.shasum)")
}
