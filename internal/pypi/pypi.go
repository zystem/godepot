package pypi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/raw"
	"github.com/zystem/godepot/internal/settings"
)

// PyPI is the handler of the repositories of the format pypi.
type PyPI struct {
	*raw.Raw
}

// New makes the handler from a raw one (which it copies and sets up).
func New(base *raw.Raw) *PyPI {
	r := *base
	p := &PyPI{Raw: &r}
	r.Self = p
	r.Layout = &raw.Layout{
		// The name is the project, normalised.
		Name: func(path string) string {
			if t, ok := parse(path); ok && t.project != "" {
				return normalize(t.project)
			}
			return path
		},
		AssetPrefix: func(name string) string { return "files/" + name + "/" },
		Also:        func(name string) []string { return []string{"index/" + name + ".json"} },
		Probe: func(path string) string {
			if t, ok := parse(path); ok && t.project != "" {
				return "simple/" + normalize(t.project) + "/"
			}
			return path
		},
		// An index can change; a file cannot.
		TTL: func(path string, u *settings.Upstream) time.Duration {
			if t, ok := parse(path); ok && t.kind == kCached {
				return u.TagTTL()
			}
			return 0
		},
		Source: p.source,
	}
	return p
}

func reply(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = fmt.Fprintln(w, msg)
}

func (p *PyPI) unavailable(w http.ResponseWriter) {
	reply(w, http.StatusServiceUnavailable, "the database or the store is not available: try again")
}

// Serve answers one request.
func (p *PyPI) Serve(w http.ResponseWriter, r *http.Request, q format.Request) {
	t, ok := parse(q.Path)
	if !ok || t.kind == kCached {
		reply(w, http.StatusNotFound, "not an address of this repository: simple/<project>/, files/<project>/<file>, legacy/ (upload)")
		return
	}
	switch q.Repo.Kind {
	case "hosted":
		p.hosted(w, r, q, t)
	case "proxy":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			reply(w, http.StatusMethodNotAllowed, "a proxy repository is read-only: upload to a hosted repository, or to a group that has write_to")
			return
		}
		switch t.kind {
		case kIndex:
			p.proxyIndex(w, r, q, t)
		case kFile:
			p.Raw.Serve(w, r, q)
		default:
			reply(w, http.StatusNotFound, "a proxy repository has no list of projects: ask for simple/<project>/")
		}
	case "group":
		p.group(w, r, q, t)
	}
}

func (p *PyPI) hosted(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	switch {
	case t.kind == kRoot && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		p.root(w, r, q)
	case t.kind == kIndex && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		p.hostedIndex(w, r, q, t)
	case t.kind == kFile && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		p.Raw.Serve(w, r, q)
	case t.kind == kFile && r.Method == http.MethodDelete:
		p.remove(w, r, q, t)
	case t.kind == kLegacy && r.Method == http.MethodPost:
		p.upload(w, r, q)
	case t.kind == kYank && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
		p.yank(w, r, q, t)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, DELETE")
		reply(w, http.StatusMethodNotAllowed, "method not allowed here")
	}
}

// group answers a request sent to a group: an upload and a yank go to write_to; an index and a file come from the member that has the project.
func (p *PyPI) group(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	write := r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete
	if t.kind == kLegacy || t.kind == kYank || (t.kind == kFile && write) {
		m := q.Set.Repo(q.Repo.WriteTo)
		if m == nil {
			w.Header().Set("Allow", "GET, HEAD")
			reply(w, http.StatusMethodNotAllowed, "this group has no write_to: upload to a hosted repository, or name one as write_to of the group")
			return
		}
		q.Repo = m
		p.Serve(w, r, q)
		return
	}
	switch t.kind {
	case kRoot:
		reply(w, http.StatusNotFound, "a group has no list of projects: ask for simple/<project>/")
	case kIndex:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			reply(w, http.StatusMethodNotAllowed, "method not allowed here")
			return
		}
		if !q.Authorize(w, auth.Read, q.Path) {
			return
		}
		ctx := r.Context()
		m, err := format.Resolve(ctx, p.DB, q.Set, q.Repo, normalize(t.project), func(l *settings.Repo) (bool, error) { return p.Raw.Has(ctx, l, "simple/"+normalize(t.project)+"/") })
		if err != nil {
			p.upstreamFailed(w, err)
			return
		}
		if m == nil {
			reply(w, http.StatusNotFound, "no member of this group has this project")
			return
		}
		q.Repo = m
		p.Serve(w, r, q)
	default:
		p.Raw.Serve(w, r, q) // a file: raw finds the member by the project, and comes back here for the member
	}
}

func (p *PyPI) upstreamFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	w.Header().Set("Retry-After", "5")
	reply(w, http.StatusServiceUnavailable, "the upstream cannot be asked now: try again ("+err.Error()+")")
}

func (p *PyPI) root(w http.ResponseWriter, r *http.Request, q format.Request) {
	if !q.Authorize(w, auth.Read, q.Path) {
		return
	}
	rows, err := p.DB.Query(r.Context(), meta.S(`SELECT path FROM assets WHERE repo = ? AND substr(path, 1, 6) = 'files/' AND substr(path, -9) <> '.metadata' LIMIT 200000`, q.Repo.Name))
	if err != nil {
		p.unavailable(w)
		return
	}
	set := map[string]bool{}
	for _, row := range rows {
		if parts := strings.Split(meta.Str(row[0]), "/"); len(parts) == 3 {
			set[parts[1]] = true
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html>\n<html>\n<head>\n<meta name=\"pypi:repository-version\" content=\"1.1\">\n<title>Simple index</title>\n</head>\n<body>\n")
	for _, n := range names {
		sb.WriteString("<a href=\"" + html.EscapeString(n) + "/\">" + html.EscapeString(n) + "</a><br/>\n")
	}
	sb.WriteString("</body>\n</html>\n")
	w.Header().Set("Content-Type", mimeHTML)
	_, _ = w.Write([]byte(sb.String()))
}

// entries reads the files of a project that a hosted repository has.
func (p *PyPI) entries(ctx context.Context, repo, project string) ([]entry, error) {
	prefix := "files/" + project + "/"
	rows, err := p.DB.Query(ctx, meta.S(`SELECT a.path, a.digest, a.size, a.created_at, c.props FROM assets a JOIN components c ON c.id = a.component_id
		WHERE a.repo = ? AND substr(a.path, 1, ?) = ? ORDER BY a.path`, repo, len(prefix), prefix))
	if err != nil {
		return nil, err
	}
	meta_ := map[string]string{} // filename -> sha256 of its .metadata
	for _, row := range rows {
		if fn, ok := strings.CutSuffix(strings.TrimPrefix(meta.Str(row[0]), prefix), ".metadata"); ok {
			meta_[fn] = meta.Str(row[1])
		}
	}
	var out []entry
	for _, row := range rows {
		fn := strings.TrimPrefix(meta.Str(row[0]), prefix)
		if strings.HasSuffix(fn, ".metadata") || strings.Contains(fn, "/") {
			continue
		}
		props := map[string]string{}
		_ = json.Unmarshal([]byte(meta.Str(row[4])), &props)
		e := entry{Filename: fn, SHA256: meta.Str(row[1]), Size: meta.Int(row[2]), RequiresPython: props["requires_python"], UploadTime: time.Unix(meta.Int(row[3]), 0).UTC().Format("2006-01-02T15:04:05.000000Z")}
		_, e.Yanked = props["yanked"]
		e.YankedReason = props["yanked_reason"]
		if sha, ok := meta_[fn]; ok {
			e.Metadata, e.MetadataSHA256 = true, sha
		}
		out = append(out, e)
	}
	return out, nil
}

func (p *PyPI) hostedIndex(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	project := normalize(t.project)
	if t.project != project {
		// PEP 503: a request for a name that is not normalised is sent to the normalised one.
		if !q.Authorize(w, auth.Read, q.Path) {
			return
		}
		http.Redirect(w, r, "/repository/"+q.Address+"/simple/"+project+"/", http.StatusMovedPermanently)
		return
	}
	if !q.Authorize(w, auth.Read, q.Path) {
		return
	}
	files, err := p.entries(r.Context(), q.Repo.Name, project)
	if err != nil {
		p.unavailable(w)
		return
	}
	if len(files) == 0 {
		reply(w, http.StatusNotFound, "no such project")
		return
	}
	render(w, r, project, files)
}

// remove deletes one file of a project: DELETE files/<project>/<filename>.
func (p *PyPI) remove(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	if strings.HasSuffix(t.filename, ".metadata") {
		reply(w, http.StatusBadRequest, "delete the file: its .metadata goes with it")
		return
	}
	if !q.Authorize(w, auth.Delete, q.Path) {
		return
	}
	code := 0
	for _, path := range []string{q.Path, q.Path + ".metadata"} {
		qq := q
		qq.Path = path
		rec := &quiet{h: http.Header{}, code: 200}
		p.Raw.Serve(rec, r, qq)
		if path == q.Path {
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
		reply(w, code, "no such file")
	default:
		p.unavailable(w)
	}
}

// yank marks a file as yanked (PEP 592): POST yank/<project>/<filename> with the reason as the body, or lifts the mark: DELETE.
func (p *PyPI) yank(w http.ResponseWriter, r *http.Request, q format.Request, t target) {
	path := "files/" + t.project + "/" + t.filename
	if !q.Authorize(w, auth.Write, path) {
		return
	}
	ctx := r.Context()
	reason := ""
	if r.Method == http.MethodPost {
		b := make([]byte, 1025)
		n, _ := r.Body.Read(b)
		if n > 1024 {
			reply(w, http.StatusBadRequest, "the reason is at most 1024 bytes")
			return
		}
		reason = strings.TrimSpace(string(b[:n]))
	}
	var st meta.Stmt
	if r.Method == http.MethodPost {
		st = meta.S(`UPDATE components SET props = json_set(props, '$.yanked', '1', '$.yanked_reason', ?) WHERE id = (SELECT component_id FROM assets WHERE repo = ? AND path = ?)`, reason, q.Repo.Name, path)
	} else {
		st = meta.S(`UPDATE components SET props = json_remove(props, '$.yanked', '$.yanked_reason') WHERE id = (SELECT component_id FROM assets WHERE repo = ? AND path = ?)`, q.Repo.Name, path)
	}
	ev := audit.Event{Actor: q.Actor(), Token: q.Principal.Token, Remote: q.Remote, Action: "yank", Repo: q.Repo.Name, Path: path, Result: audit.OK, Detail: r.Method + " " + reason}
	if _, found, err := p.Lookup(ctx, q.Repo.Name, path); err != nil {
		p.unavailable(w)
		return
	} else if !found {
		reply(w, http.StatusNotFound, "no such file")
		return
	}
	if _, err := p.DB.Exec(ctx, st, p.Audit.Stmt(ev)); err != nil {
		p.unavailable(w)
		return
	}
	p.Audit.Emit(ev)
	w.WriteHeader(http.StatusNoContent)
}

// quiet takes an answer that is not for a client.
type quiet struct {
	h    http.Header
	code int
}

func (q *quiet) Header() http.Header         { return q.h }
func (q *quiet) WriteHeader(c int)           { q.code = c }
func (q *quiet) Write(b []byte) (int, error) { return len(b), nil }
