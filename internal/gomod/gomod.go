package gomod

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/raw"
	"github.com/zystem/godepot/internal/settings"
)

// The limits of a module that is published (docs/spec.md, F5): those of the go command itself, which refuses a larger module.
const (
	MaxZip         = 500 << 20 // the zip, as it is sent
	maxUnzipped    = 500 << 20 // all the files together, as the zip says they are
	maxGoMod       = 16 << 20
	maxEntries     = 20000
	infoTimeLayout = "2006-01-02T15:04:05Z"
)

// Go is the handler of the repositories of the format go.
type Go struct {
	*raw.Raw
}

// New makes the handler from a raw one (which it copies and sets up for module paths).
func New(base *raw.Raw) *Go {
	r := *base
	g := &Go{Raw: &r}
	r.Self = g
	r.Layout = &raw.Layout{
		Unit: func(path string) (string, string, bool) {
			if t, ok := parse(path); ok && t.kind != kList && t.kind != kLatest {
				return t.module, t.version, true
			}
			return "", "", false
		},
		Name: func(path string) string {
			if t, ok := parse(path); ok {
				return t.module
			}
			return path
		},
		Probe: func(path string) string {
			if t, ok := parse(path); ok {
				return t.module + "/@v/list"
			}
			return path
		},
		// A version never changes; the list and the latest do.
		TTL: func(path string, u *settings.Upstream) time.Duration {
			if t, ok := parse(path); ok && (t.kind == kList || t.kind == kLatest) {
				return u.TagTTL()
			}
			return 0
		},
	}
	return g
}

func reply(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg+"\n")
}

// Serve answers one request.
func (g *Go) Serve(w http.ResponseWriter, r *http.Request, q format.Request) {
	t, ok := parse(q.Path)
	if !ok {
		reply(w, http.StatusNotFound, "not a path of the module proxy protocol: <module>/@v/list, <module>/@v/<version>.info|.mod|.zip, <module>/@latest")
		return
	}
	if q.Repo.Kind != "hosted" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && q.Repo.Kind == "proxy" {
			w.Header().Set("Allow", "GET, HEAD")
			reply(w, http.StatusMethodNotAllowed, "a proxy repository is read-only")
			return
		}
		if q.Repo.Kind == "proxy" && g.policed(w, r, q, t) {
			return // the cooldown and the lists answered
		}
		g.Raw.Serve(w, r, q) // a proxy fetches and keeps; a group finds the member that has the module
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		switch t.kind {
		case kList:
			g.list(w, r, q, t)
		case kLatest:
			g.latest(w, r, q, t)
		default:
			g.Raw.Serve(w, r, q)
		}
	case http.MethodPut:
		g.publish(w, r, q, t)
	case http.MethodDelete:
		g.unpublish(w, r, q, t)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		reply(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// versions lists the versions of a module that a hosted repository has: those that have a zip.
func (g *Go) versions(ctx context.Context, repo, module string) ([]string, error) {
	prefix := module + "/@v/"
	rows, err := g.DB.Query(ctx, meta.S(`SELECT path FROM assets WHERE repo = ? AND substr(path, 1, ?) = ? AND substr(path, -4) = '.zip'`, repo, len(prefix), prefix))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range rows {
		v := strings.TrimSuffix(strings.TrimPrefix(meta.Str(row[0]), prefix), ".zip")
		if u, ok := unescape(v); ok {
			if _, ok := parseSemver(u); ok {
				out = append(out, u)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := parseSemver(out[i])
		b, _ := parseSemver(out[j])
		return a.less(b)
	})
	return out, nil
}

func (g *Go) unavailable(w http.ResponseWriter) {
	reply(w, http.StatusServiceUnavailable, "the database or the store is not available: try again")
}

func (g *Go) list(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Read, q.Path) {
		return
	}
	vs, err := g.versions(r.Context(), q.Repo.Name, t.module)
	if err != nil {
		g.unavailable(w)
		return
	}
	if len(vs) == 0 {
		reply(w, http.StatusNotFound, "no such module")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, strings.Join(vs, "\n")+"\n")
	}
}

// latest answers with the info of the highest version: a release when there is one, else the highest prerelease.
func (g *Go) latest(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if !q.Authorize(w, auth.Read, q.Path) {
		return
	}
	vs, err := g.versions(r.Context(), q.Repo.Name, t.module)
	if err != nil {
		g.unavailable(w)
		return
	}
	best, release := "", ""
	for _, v := range vs { // in ascending order
		best = v
		if s, _ := parseSemver(v); len(s.pre) == 0 {
			release = v
		}
	}
	if release != "" {
		best = release // a prerelease is the latest only when there is no release
	}
	if best == "" {
		reply(w, http.StatusNotFound, "no such module")
		return
	}
	q.Path = t.module + "/@v/" + escape(best) + ".info"
	g.Raw.Serve(w, r, q)
}

// checkZip reads a module zip without unpacking it anywhere: every name must be under "<module>@<version>/", no name may climb out or repeat, the files may add up to no more than the go command takes,
// and the go.mod in it must name the module. It returns the go.mod.
func checkZip(ctx context.Context, l *blob.Layer, digest string, size int64, module, version string) ([]byte, error) {
	zr, err := zip.NewReader(format.BlobReaderAt{Ctx: ctx, L: l, Digest: digest}, size)
	if err != nil {
		return nil, errors.New("the body is not a zip")
	}
	if len(zr.File) > maxEntries {
		return nil, fmt.Errorf("the zip has more than %d files", maxEntries)
	}
	prefix := module + "@" + version + "/"
	seen := map[string]bool{}
	var total uint64
	var gomod *zip.File
	for _, f := range zr.File {
		n := f.Name
		switch {
		case !strings.HasPrefix(n, prefix):
			return nil, fmt.Errorf("the file %q is not under %q", n, prefix)
		case strings.Contains(n, "\\") || strings.Contains(n, "\x00") || strings.Contains(n, "//"):
			return nil, fmt.Errorf("the name %q is not allowed", n)
		case seen[n]:
			return nil, fmt.Errorf("the file %q is in the zip twice", n)
		}
		for _, part := range strings.Split(strings.TrimSuffix(n, "/"), "/") {
			if part == ".." || part == "." {
				return nil, fmt.Errorf("the name %q has a %q part", n, part)
			}
		}
		seen[n] = true
		if strings.HasSuffix(n, "/") {
			continue
		}
		total += f.UncompressedSize64
		if total > maxUnzipped {
			return nil, fmt.Errorf("the files of the zip are over %d bytes", maxUnzipped)
		}
		if n == prefix+"go.mod" {
			gomod = f
		}
	}
	if gomod == nil {
		return nil, errors.New("the zip has no go.mod at its root")
	}
	if gomod.UncompressedSize64 > maxGoMod {
		return nil, errors.New("the go.mod is too large")
	}
	rc, err := gomod.Open()
	if err != nil {
		return nil, errors.New("the go.mod cannot be read")
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxGoMod+1))
	if err != nil || len(b) > maxGoMod {
		return nil, errors.New("the go.mod cannot be read")
	}
	if got := modulePath(b); got != module {
		return nil, fmt.Errorf("the go.mod names the module %q, the address %q", got, module)
	}
	return b, nil
}

// modulePath reads the module directive of a go.mod ("" when there is none).
func modulePath(gomod []byte) string {
	for _, line := range strings.Split(string(gomod), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		rest, ok := strings.CutPrefix(line, "module")
		if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		rest = strings.TrimSpace(rest)
		if len(rest) >= 2 && (rest[0] == '"' || rest[0] == '`') && rest[len(rest)-1] == rest[0] {
			rest = rest[1 : len(rest)-1]
		}
		return rest
	}
	return ""
}

// majorOK says whether the module path has the major version suffix that the version needs: none for v0 and v1, "/vN" for vN (N from 2), unless the version is "+incompatible".
func majorOK(module, version string) bool {
	s, _ := parseSemver(version)
	suffix := ""
	if i := strings.LastIndex(module, "/"); i >= 0 {
		suffix = module[i+1:]
	}
	isMajor := len(suffix) >= 2 && suffix[0] == 'v' && strings.Trim(suffix[1:], "0123456789") == ""
	switch {
	case strings.HasSuffix(version, "+incompatible"):
		return !isMajor
	case s.nums[0] >= 2:
		return suffix == fmt.Sprintf("v%d", s.nums[0])
	default:
		return !isMajor
	}
}

// quiet takes an answer that is not for a client.
type quiet struct {
	h    http.Header
	code int
}

func (q *quiet) Header() http.Header         { return q.h }
func (q *quiet) WriteHeader(c int)           { q.code = c }
func (q *quiet) Write(b []byte) (int, error) { return len(b), nil }

// publish takes a module zip: PUT <module>/@v/<version>.zip. The .mod and .info are made from it, so they are not put.
func (g *Go) publish(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if t.kind != kZip {
		reply(w, http.StatusBadRequest, "put the .zip of the version: its .mod and .info are made from it")
		return
	}
	if !q.Authorize(w, auth.Write, q.Path) {
		return
	}
	ctx := r.Context()
	module, _ := unescape(t.module)
	version, _ := unescape(t.version)
	if !majorOK(module, version) {
		reply(w, http.StatusBadRequest, fmt.Sprintf("the version %s does not go with the module path %s (v2 and later need a /vN suffix, v0 and v1 none)", version, module))
		return
	}
	up, err := g.Blobs.Ingest(ctx, http.MaxBytesReader(w, r.Body, MaxZip), "")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) || strings.Contains(err.Error(), "request body too large") {
			reply(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a module zip is at most %d bytes", MaxZip))
			return
		}
		g.unavailable(w)
		return
	}
	gomod, err := checkZip(ctx, g.Blobs, up.Digest, up.Size, module, version)
	if err != nil {
		_ = g.Blobs.Abandon(ctx, up)
		reply(w, http.StatusBadRequest, err.Error())
		return
	}
	// The same bytes again is fine and changes nothing; other bytes under a version are refused (docs/spec.md, R4).
	if have, found, err := g.Lookup(ctx, q.Repo.Name, q.Path); err != nil {
		_ = g.Blobs.Abandon(ctx, up)
		g.unavailable(w)
		return
	} else if found {
		_ = g.Blobs.Abandon(ctx, up)
		if have == up.Digest {
			w.Header().Set("X-Checksum-Sha256", up.Digest)
			reply(w, http.StatusOK, "the version is there with these bytes")
		} else {
			reply(w, http.StatusConflict, "this version is published with other bytes; a version is not overwritten: publish a new one")
		}
		return
	}
	base := t.module + "/@v/" + t.version
	info, _ := json.Marshal(map[string]string{"Version": version, "Time": time.Now().UTC().Format(infoTimeLayout)})
	for _, f := range []struct {
		ext, ct string
		body    []byte
	}{{".mod", "text/plain; charset=utf-8", gomod}, {".info", "application/json", info}} {
		in, err := g.Blobs.Ingest(ctx, bytes.NewReader(f.body), "")
		if err != nil {
			_ = g.Blobs.Abandon(ctx, up)
			g.unavailable(w)
			return
		}
		qq := &quiet{h: http.Header{}, code: 200}
		g.Raw.Commit(qq, ctx, q, base+f.ext, in, f.ct, "{}")
		if qq.code >= 500 {
			_ = g.Blobs.Abandon(ctx, up)
			g.unavailable(w)
			return
		}
		// 409: an earlier publish of this version that did not end left its .mod or .info; the zip decides.
	}
	g.Raw.Commit(w, ctx, q, q.Path, up, "application/zip", "{}")
}

// unpublish removes a version: DELETE <module>/@v/<version>.zip removes the zip, the .mod and the .info.
func (g *Go) unpublish(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if t.kind != kZip {
		reply(w, http.StatusBadRequest, "delete the .zip of the version: its .mod and .info go with it")
		return
	}
	base := t.module + "/@v/" + t.version
	if !q.Authorize(w, auth.Delete, base+".zip") {
		return
	}
	// The zip goes first: the version is then out of the list at once; what is left is a .mod and an .info that nothing lists.
	code := 0
	for _, ext := range []string{".zip", ".mod", ".info"} {
		qq := q
		qq.Path = base + ext
		rec := &quiet{h: http.Header{}, code: 200}
		g.Raw.Serve(rec, r, qq)
		if ext == ".zip" {
			code = rec.code
			if code != http.StatusNoContent {
				break
			}
		}
	}
	switch code {
	case http.StatusNoContent:
		w.WriteHeader(code)
	case http.StatusNotFound:
		reply(w, code, "no such version")
	default:
		g.unavailable(w)
	}
}
