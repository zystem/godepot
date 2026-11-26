package pypi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/policy"
	"github.com/zystem/godepot/internal/raw"
)

const maxIndex = 64 << 20 // the largest index of an upstream that is read

// upIndex is what is read of an index in the JSON of PEP 691.
type upIndex struct {
	Files []struct {
		Filename         string            `json:"filename"`
		URL              string            `json:"url"`
		Hashes           map[string]string `json:"hashes"`
		RequiresPython   string            `json:"requires-python"`
		Yanked           json.RawMessage   `json:"yanked"`
		CoreMetadata     json.RawMessage   `json:"core-metadata"`
		DistInfoMetadata json.RawMessage   `json:"dist-info-metadata"`
		Size             int64             `json:"size"`
		UploadTime       string            `json:"upload-time"`
	} `json:"files"`
}

// capture takes an answer into memory (the cached index of a proxy), up to maxIndex.
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

// cached gets the index of a project from the cache of a proxy repository, which asks the upstream when the copy is missing or past its time to live (and serves it stale when the upstream cannot
// answer): the answer of raw for the path index/<project>.json, taken into memory. It returns that answer's status when it is not 200.
func (p *PyPI) cached(ctx context.Context, r *http.Request, q format.Request, project string) (*upIndex, *capture, error) {
	qq := q
	qq.Path = "index/" + project + ".json"
	qq.Authorize = func(http.ResponseWriter, string, string) bool { return true } // the request was authorized for the project
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
	var idx upIndex
	if err := json.Unmarshal(c.buf.Bytes(), &idx); err != nil {
		return nil, nil, errors.New("the upstream's index is not the JSON of PEP 691 (it must answer Accept: " + mimeJSON + ")")
	}
	return &idx, c, nil
}

func (p *PyPI) proxyIndex(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	project := normalize(t.project)
	if !q.Authorize(w, auth.Read, q.Path) {
		return
	}
	if t.project != project {
		http.Redirect(w, r, "/repository/"+q.Address+"/simple/"+project+"/", http.StatusMovedPermanently)
		return
	}
	idx, c, err := p.cached(r.Context(), r, q, project)
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	if idx == nil {
		for _, k := range []string{"Retry-After"} {
			if v := c.h.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		code := c.code
		if code == 0 || c.full {
			code = http.StatusBadGateway
		}
		reply(w, code, "the index of this project is not available: "+strings.TrimSpace(c.buf.String()))
		return
	}
	if v := c.h.Get("Warning"); v != "" {
		w.Header().Set("Warning", v) // the copy is stale: the upstream could not be asked
	}
	var files []entry
	seen := map[string]bool{}
	pol := q.Repo.Upstream.Policy()
	for _, f := range idx.Files {
		fp, ok := parseFilename(f.Filename)
		sha := strings.ToLower(f.Hashes["sha256"])
		if !ok || fp.project != project || len(sha) != 64 || seen[f.Filename] {
			continue
		}
		if !pol.Version(project, fp.version, uploaded(f.UploadTime), p.now()).OK || pol.BlockedDigest("sha256:"+sha) {
			continue // held back by the cooldown or the block list: not in the index
		}
		seen[f.Filename] = true
		e := entry{Filename: f.Filename, SHA256: sha, Size: f.Size, RequiresPython: f.RequiresPython, UploadTime: f.UploadTime}
		if y := strings.TrimSpace(string(f.Yanked)); y != "" && y != "false" {
			e.Yanked = true
			var reason string
			if json.Unmarshal(f.Yanked, &reason) == nil {
				e.YankedReason = reason
			}
		}
		for _, m := range []json.RawMessage{f.CoreMetadata, f.DistInfoMetadata} {
			if y := strings.TrimSpace(string(m)); y != "" && y != "false" && !e.Metadata {
				e.Metadata = true
				var h map[string]string
				if json.Unmarshal(m, &h) == nil {
					e.MetadataSHA256 = strings.ToLower(h["sha256"])
				}
			}
		}
		files = append(files, e)
	}
	if len(files) == 0 {
		reply(w, http.StatusNotFound, "no such project")
		return
	}
	render(w, r, project, files)
}

// source says where a proxied file comes from: the index of a project is the upstream's simple page asked for in JSON; a file is at the address that index names, with the hash it names.
func (p *PyPI) source(ctx context.Context, h *raw.Raw, q format.Request, path string) (*raw.Source, error) {
	t, ok := parse(path)
	if !ok {
		return nil, raw.ErrNoSource
	}
	switch t.kind {
	case kCached:
		return &raw.Source{Path: "simple/" + t.project + "/", Header: http.Header{"Accept": {mimeJSON}}}, nil
	case kFile:
	default:
		return nil, raw.ErrNoSource
	}
	// A file: the address is in the index. The index is read from the cache (and fetched when it is not there).
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	idx, c, err := p.cached(ctx, r, q, t.project)
	if err != nil {
		return nil, err
	}
	if idx == nil {
		if c != nil && (c.code == http.StatusNotFound || c.code == 0) {
			return nil, raw.ErrNoSource
		}
		return nil, errors.New("the index of the project is not available")
	}
	want, meta := strings.CutSuffix(t.filename, ".metadata")
	for _, f := range idx.Files {
		if f.Filename != want {
			continue
		}
		s := &raw.Source{SHA256: strings.ToLower(f.Hashes["sha256"])}
		u := f.URL
		if meta {
			u += ".metadata"
			s.SHA256 = ""
			for _, m := range []json.RawMessage{f.CoreMetadata, f.DistInfoMetadata} {
				var hh map[string]string
				if json.Unmarshal(m, &hh) == nil && len(hh["sha256"]) == 64 {
					s.SHA256 = strings.ToLower(hh["sha256"])
				}
			}
		}
		if strings.HasPrefix(u, "/") {
			s.Path = strings.TrimPrefix(u, "/") // a path on the upstream itself
		} else {
			s.URL = u
		}
		return s, nil
	}
	return nil, raw.ErrNoSource
}

func (p *PyPI) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// uploaded reads the time of an upload in an index (the zero time when there is none).
func uploaded(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// fileAllowed sees that the policy lets a file of a proxy go: the block list by name and version, then the cooldown against the time in the index. It answers the client when not.
func (p *PyPI) fileAllowed(w http.ResponseWriter, r *http.Request, q format.Request, t target) bool {
	pol := q.Repo.Upstream.Policy()
	if !pol.Active() {
		return true
	}
	fp, ok := parseFilename(strings.TrimSuffix(t.filename, ".metadata"))
	if !ok {
		return true
	}
	refuse := func(v policy.Verdict) bool {
		if !v.Until.IsZero() {
			w.Header().Set("Retry-After", strconv.FormatInt(max(1, int64(time.Until(v.Until).Seconds())), 10))
		}
		reply(w, http.StatusForbidden, v.Why)
		return false
	}
	if v, blocked := pol.Blocked(t.project, fp.version); blocked {
		return refuse(v)
	}
	if pol.Cooldown <= 0 && !hasDigestRule(pol) {
		return true
	}
	idx, c, err := p.cached(r.Context(), r, q, t.project)
	if err != nil || idx == nil {
		code := http.StatusBadGateway
		if c != nil && c.code == http.StatusNotFound {
			code = http.StatusNotFound
		}
		reply(w, code, "the index of the project is not available, so the cooldown cannot be checked")
		return false
	}
	want := strings.TrimSuffix(t.filename, ".metadata")
	for _, f := range idx.Files {
		if f.Filename == want {
			if pol.BlockedDigest("sha256:" + strings.ToLower(f.Hashes["sha256"])) {
				return refuse(policy.Verdict{Blocked: true, Why: want + " is on the block list of this repository (by its digest)"})
			}
			if v := pol.Version(t.project, fp.version, uploaded(f.UploadTime), p.now()); !v.OK {
				return refuse(v)
			}
			return true
		}
	}
	return true // a file the index does not list is a 404 of raw's
}

func hasDigestRule(p *policy.Policy) bool {
	for _, r := range p.Block {
		if r.Digest != "" {
			return true
		}
	}
	return false
}
