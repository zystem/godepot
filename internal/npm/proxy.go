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
	"strconv"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/policy"
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
	// Time is when each version was published (the full packument has it; the abbreviated has not).
	Time map[string]string `json:"time"`
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
	root := base(r, q)
	pol := q.Repo.Upstream.Policy()
	versions := map[string]json.RawMessage{}
	for v, raw := range d.Versions {
		var uv upVersion
		if json.Unmarshal(raw, &uv) != nil || uv.Name != t.pkg || uv.Version != v || !validVersion(v) {
			continue
		}
		if !pol.Version(t.pkg, v, published(d, v), p.now()).OK {
			continue // held back by the cooldown or the block list: not in the document
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
	if _, ok := tags["latest"]; !ok && d.DistTags["latest"] != "" {
		// The latest is held back: the highest version that is left (and is no prerelease, if any is not) takes its place, so that a plain install gets something.
		if v := highest(versions); v != "" {
			tags["latest"] = v
		}
	}
	if t.kind == kTags {
		b, _ := json.Marshal(tags)
		send(w, r, b, false, extra)
		return
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
		if q.Repo.Upstream != nil && q.Repo.Upstream.Cooldown != nil {
			// A cooldown needs the time of each version, which only the full packument has.
			return &raw.Source{Path: t.pkg, Header: http.Header{"Accept": {mimeJSON}}}, nil
		}
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

func (p *NPM) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// published is when the upstream published the version (the zero time when its packument does not say).
func published(d *upDoc, v string) time.Time {
	t, err := time.Parse(time.RFC3339, d.Time[v])
	if err != nil {
		return time.Time{}
	}
	return t
}

// refuse answers a request for a version that the policy holds back, 403 with the words of the verdict (a client that asks by name is told why and when).
func refuse(w http.ResponseWriter, v policy.Verdict) {
	if !v.Until.IsZero() {
		w.Header().Set("Retry-After", strconv.FormatInt(max(1, int64(time.Until(v.Until).Seconds())), 10))
	}
	fail(w, http.StatusForbidden, v.Why)
}

// tarballAllowed sees that the policy lets the tarball of a version of a proxy go: the block list by name, then the cooldown against the time in the packument. It answers the client when not.
func (p *NPM) tarballAllowed(w http.ResponseWriter, r *http.Request, q format.Request, t target) bool {
	pol := q.Repo.Upstream.Policy()
	if !pol.Active() {
		return true
	}
	v, _ := version(t.pkg, t.file)
	if verdict, blocked := pol.Blocked(t.pkg, v); blocked {
		refuse(w, verdict)
		return false
	}
	if pol.Cooldown <= 0 {
		return true
	}
	d, c, err := p.cached(r.Context(), r, q, t.pkg)
	if err != nil || d == nil {
		code := http.StatusBadGateway
		if c != nil && c.code == http.StatusNotFound {
			code = http.StatusNotFound
		}
		fail(w, code, "the packument of the package is not available, so the cooldown cannot be checked")
		return false
	}
	if verdict := pol.Version(t.pkg, v, published(d, v), p.now()); !verdict.OK {
		refuse(w, verdict)
		return false
	}
	return true
}

// highest is the highest version of the set: of those with no prerelease when there are some.
func highest(versions map[string]json.RawMessage) string {
	best, bestPre := "", ""
	for v := range versions {
		if strings.Contains(v, "-") {
			if bestPre == "" || semverLess(bestPre, v) {
				bestPre = v
			}
			continue
		}
		if best == "" || semverLess(best, v) {
			best = v
		}
	}
	if best == "" {
		return bestPre
	}
	return best
}

// semverLess orders two versions by their numbers (a prerelease is not told from a release: the callers have separated them).
func semverLess(a, b string) bool {
	pa, pb := nums(a), nums(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return a < b
}

func nums(v string) [3]int {
	v, _, _ = strings.Cut(v, "+")
	v, _, _ = strings.Cut(v, "-")
	var out [3]int
	for i, s := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(s)
		out[i] = n
	}
	return out
}
