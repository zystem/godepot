// Package raw is the format "raw": files by path, as they are. PUT writes a file, GET and HEAD read it (with ranges), DELETE removes it, and a GET of a directory (a path that ends with "/")
// lists the files below it. It is the smallest format and the first one built end to end: the paths, the immutability (docs/spec.md, R4), the properties (R6), the quota (R7), the audit (A6).
package raw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
	"github.com/zystem/godepot/internal/metrics"
	"github.com/zystem/godepot/internal/settings"
	"github.com/zystem/godepot/internal/trash"
	"github.com/zystem/godepot/internal/upstream"
)

// Raw is the handler of the repositories of the format raw.
type Raw struct {
	Blobs *blob.Layer
	DB    meta.DB
	Audit *audit.Log
	// Reads, Writes and Deletes count the requests per repository; Refused counts by reason. nil: not counted.
	Reads, Writes, Deletes, Refused *metrics.Counter
	// Downloads limits the files being sent at once (nil: no limit); Rate is the least speed of a client in bytes a second (0: none); Active shows how many are going on (docs/spec.md, P11).
	Downloads *format.Gate
	Rate      int64
	Active    *metrics.Gauge
	// Layout says how a format built on this one names things (nil: the format raw itself).
	Layout *Layout
	// Self is the format that serves the requests that this one hands on (a group hands the request to the member that answers): nil is this one. A format built on raw sets it to itself.
	Self format.Format
	// Up holds the clients of the upstreams of the proxy repositories (nil: there are none).
	Up *upstream.Pool
	// LastRead notes the components that are read (C4); nil: not kept. Trash is where a delete goes (C3); nil: a delete frees at once.
	LastRead *format.ReadLog
	Trash    *trash.Trash
	// Now is the clock (default time.Now).
	Now func() time.Time
}

const (
	maxPath      = 1024
	maxProps     = 32
	maxPropValue = 1024
	propHeader   = "X-Godepot-Property-"
	maxTries     = 32
)

var propKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// CheckPath says what is wrong with the path of a file: no empty, "." or ".." parts, no control characters, valid UTF-8, not too long.
func CheckPath(p string) error {
	if p == "" || len(p) > maxPath {
		return fmt.Errorf("the path must be 1 to %d bytes", maxPath)
	}
	if !utf8.ValidString(p) {
		return errors.New("the path is not valid UTF-8")
	}
	if strings.HasSuffix(p, "/") {
		return errors.New("a file path does not end with \"/\"")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("the path has an empty, \".\" or \"..\" part")
		}
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return errors.New("the path has a control character or a backslash")
		}
	}
	return nil
}

type problem struct {
	Error string `json:"error"`
	Info  any    `json:"info,omitempty"`
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string, info any) { reply(w, code, problem{msg, info}) }

func (h *Raw) count(c *metrics.Counter, label string) {
	if c != nil {
		c.Add(label, 1)
	}
}

// Layout is what a format built on raw changes in it: which name claims and groups work with, how a proxy asks whether a name exists, and how long a proxied file is trusted.
type Layout struct {
	// Name is the name that a path belongs to (a module, an artifact); a group answers for a name from one member and a published name is claimed. nil: the name is the path.
	Name func(path string) string
	// Probe is the path to ask an upstream, to know whether it has the name of the path. nil: the path itself.
	Probe func(path string) string
	// AssetPrefix is the prefix of the paths of the files of a name, when they are not at "<name>/" (PyPI: "files/<project>/"). nil: "<name>/".
	AssetPrefix func(name string) string
	// Also lists, for a name, paths that show that the repository has it besides the files below it (PyPI: the cached index of a proxy). nil: none.
	Also func(name string) []string
	// Source says where a proxied file comes from when it is not at the same path of the upstream, and what the upstream names of it (PyPI: the address in an index, and its hash). nil: the same path.
	Source func(ctx context.Context, h *Raw, q format.Request, path string) (*Source, error)
	// TTL is how long a proxied file is trusted: 0 is for ever. nil: the setting ttl of the upstream (none by default).
	TTL func(path string, u *settings.Upstream) time.Duration
	// Unit says which version a file belongs to, for the cleanup (docs/spec.md, C1): the files of a name and a version are removed together. ok is false for a file that is not part of a version (a list of
	// versions, a cached index): a policy never removes it. nil: every file is a version of its own, named by its path.
	Unit func(path string) (name, version string, ok bool)
}

// Unit is the name and the version that the cleanup counts the file at the path in.
func (h *Raw) Unit(path string) (string, string, bool) {
	if h.Layout != nil && h.Layout.Unit != nil {
		return h.Layout.Unit(path)
	}
	return path, "", true
}

// Source is where a proxied file is fetched from.
type Source struct {
	URL    string      // an address the upstream named, or "" for Path on the upstream
	Path   string      // a path on the upstream (with no leading slash)
	Header http.Header // sent with the request
	SHA256 string      // the digest the upstream names, in hex ("" for none)
	// Also is a hash of another kind that the upstream names (npm names a sha512 and a sha1): the bytes are checked against it before the last byte is let go, as against SHA256. nil: none.
	Also *Hash
}

// Hash is a digest of a kind that the store does not use, to be checked of what a proxy fetches.
type Hash struct {
	New func() hash.Hash
	Sum []byte
}

// ErrNoSource is what Layout.Source returns for a path that the upstream does not have.
var ErrNoSource = errors.New("the upstream has no such file")

func (h *Raw) self() format.Format {
	if h.Self != nil {
		return h.Self
	}
	return h
}

// name is the name that claims and groups work with for the path.
func (h *Raw) name(path string) string {
	if h.Layout != nil && h.Layout.Name != nil {
		return h.Layout.Name(path)
	}
	return path
}

func (h *Raw) ttl(u *settings.Upstream, path string) time.Duration {
	if h.Layout != nil && h.Layout.TTL != nil {
		return h.Layout.TTL(path, u)
	}
	return u.FileTTL()
}

// Serve answers one request.
func (h *Raw) Serve(w http.ResponseWriter, r *http.Request, q format.Request) {
	switch q.Repo.Kind {
	case "group":
		h.group(w, r, q)
		return
	case "proxy":
		h.proxy(w, r, q)
		return
	}
	switch r.Method {
	case http.MethodPut:
		h.put(w, r, q)
	case http.MethodGet, http.MethodHead:
		if q.Path == "" || strings.HasSuffix(q.Path, "/") {
			h.list(w, r, q)
		} else {
			h.get(w, r, q)
		}
	case http.MethodDelete:
		h.delete(w, r, q)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		fail(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

// asset is a row of assets with the properties of its component.
type asset struct {
	id, component, digest, contentType, etag string
	size, created, checked                   int64
	props                                    map[string]string
}

func (h *Raw) find(ctx context.Context, repo, path string) (*asset, error) {
	rows, err := h.DB.Query(ctx, meta.S(`SELECT a.id, a.component_id, a.digest, a.size, a.content_type, a.created_at, c.props, a.etag, a.checked_at
		FROM assets a JOIN components c ON c.id = a.component_id WHERE a.repo = ? AND a.path = ?`, repo, path))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	a := &asset{id: meta.Str(r[0]), component: meta.Str(r[1]), digest: meta.Str(r[2]), size: meta.Int(r[3]), contentType: meta.Str(r[4]), created: meta.Int(r[5]), props: map[string]string{}, etag: meta.Str(r[7]), checked: meta.Int(r[8])}
	_ = json.Unmarshal([]byte(meta.Str(r[6])), &a.props)
	return a, nil
}

// properties reads the properties from the headers of an upload.
func properties(r *http.Request) (map[string]string, error) {
	props := map[string]string{}
	for name, vals := range r.Header {
		if !strings.HasPrefix(name, propHeader) {
			continue
		}
		key := strings.ToLower(strings.TrimPrefix(name, propHeader))
		if !propKeyRE.MatchString(key) {
			return nil, fmt.Errorf("the property name %q is not allowed: 1 to 64 of a-z, 0-9, '.', '_', '-'", key)
		}
		v := strings.Join(vals, ",")
		if len(v) > maxPropValue || !utf8.ValidString(v) {
			return nil, fmt.Errorf("the value of property %q is longer than %d bytes or not UTF-8", key, maxPropValue)
		}
		props[key] = v
	}
	if len(props) > maxProps {
		return nil, fmt.Errorf("more than %d properties", maxProps)
	}
	return props, nil
}

func remoteOf(q format.Request) string { return q.Remote }

func (h *Raw) event(q format.Request, action, path, result, detail string) audit.Event {
	actor, token := "anonymous", ""
	if !q.Principal.Anonymous() {
		actor, token = q.Principal.Account, q.Principal.Token
	}
	return audit.Event{Actor: actor, Token: token, Remote: remoteOf(q), Action: action, Repo: q.Repo.Name, Path: path, Result: result, Detail: detail}
}

func (h *Raw) put(w http.ResponseWriter, r *http.Request, q format.Request) {
	path := q.Path
	if err := CheckPath(path); err != nil {
		fail(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if !q.Authorize(w, auth.Write, path) {
		return
	}
	props, err := properties(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	want := strings.ToLower(r.Header.Get("X-Checksum-Sha256"))
	if want != "" && !blob.ValidDigest(want) {
		fail(w, http.StatusBadRequest, "X-Checksum-Sha256 is not a SHA-256 in hex", nil)
		return
	}
	ctx := r.Context()
	// Before reading a large body: a path that is there with other bytes, in a repository that does not overwrite, and a body that is known to be too big.
	if old, err := h.find(ctx, q.Repo.Name, path); err != nil {
		h.unavailable(w, err)
		return
	} else if old != nil && want != "" && old.digest != want && !q.Repo.Overwrites(path) {
		h.conflict(w, q, path, old.digest)
		return
	}
	if q.Repo.QuotaBytes > 0 && r.ContentLength > q.Repo.QuotaBytes {
		h.overQuota(w, q, path, "quota_bytes")
		return
	}
	up, err := h.Blobs.Ingest(ctx, r.Body, want)
	switch {
	case errors.Is(err, blob.ErrDigest):
		h.count(h.Refused, "digest")
		h.Audit.Record(ctx, h.event(q, "put", path, audit.Refused, "the bytes do not have the digest named in X-Checksum-Sha256"))
		fail(w, http.StatusBadRequest, "the bytes do not have the SHA-256 named in X-Checksum-Sha256", nil)
		return
	case err != nil:
		h.unavailable(w, err)
		return
	}
	ct := r.Header.Get("Content-Type")
	if len(ct) > 255 {
		ct = ct[:255]
	}
	pj, _ := json.Marshal(props)
	h.commit(w, ctx, q, path, up, ct, string(pj))
}

// Commit makes a file of an upload that Ingest has taken in: the same as a PUT after its body has been read, for a format that reads the bytes first (it checks them) and then says which path they
// are. It answers the client. props is a JSON object ("{}" for none).
func (h *Raw) Commit(w http.ResponseWriter, ctx context.Context, q format.Request, path string, up *blob.Upload, contentType, props string) {
	h.commit(w, ctx, q, path, up, contentType, props)
}

func (h *Raw) commit(w http.ResponseWriter, ctx context.Context, q format.Request, path string, up *blob.Upload, ct, pj string) {

	for try := 0; try < maxTries; try++ {
		old, err := h.find(ctx, q.Repo.Name, path)
		if err != nil {
			h.unavailable(w, err)
			return
		}
		if old != nil && old.digest == up.Digest {
			_ = h.Blobs.Abandon(ctx, up) // the same bytes again: nothing to change
			h.Audit.Record(ctx, h.event(q, "put", path, audit.OK, "unchanged "+up.Digest))
			h.done(w, http.StatusOK, path, up)
			return
		}
		if old != nil && !q.Repo.Overwrites(path) {
			_ = h.Blobs.Abandon(ctx, up)
			h.conflict(w, q, path, old.digest)
			return
		}
		id := format.Token()
		owner := "asset:" + id
		stmts := h.Blobs.Hold(owner, up.Digest)
		if q.Repo.QuotaBytes > 0 {
			var oldSize int64
			if old != nil {
				oldSize = old.size
			}
			stmts = append(stmts, meta.Unless("quota_bytes", `SELECT 1 WHERE (SELECT COALESCE(SUM(size), 0) FROM assets WHERE repo = ?) - ? + ? > ?`, q.Repo.Name, oldSize, up.Size, q.Repo.QuotaBytes))
		}
		var component string
		if old != nil {
			component = old.component
			stmts = append(stmts,
				meta.Guard("changed", `SELECT 1 FROM assets WHERE id = ? AND digest = ?`, old.id, old.digest))
			stmts = append(stmts, h.Blobs.Drop("asset:"+old.id)...)
			stmts = append(stmts,
				meta.S(`DELETE FROM assets WHERE id = ?`, old.id),
				meta.S(`UPDATE components SET props = ?, created_by = ?, created_at = ? WHERE id = ?`, string(pj), q.Actor(), h.nowUnix(), component))
		} else {
			component = format.Token()
			if q.Repo.QuotaComponents > 0 {
				stmts = append(stmts, meta.Unless("quota_components", `SELECT 1 WHERE (SELECT COUNT(*) FROM components WHERE repo = ?) >= ?`, q.Repo.Name, q.Repo.QuotaComponents))
			}
			stmts = append(stmts,
				meta.Unless("changed", `SELECT 1 FROM assets WHERE repo = ? AND path = ?`, q.Repo.Name, path),
				meta.S(`INSERT INTO components (id, repo, name, props, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`, component, q.Repo.Name, path, string(pj), q.Actor(), h.nowUnix()))
		}
		stmts = append(stmts,
			meta.S(`INSERT INTO assets (id, component_id, repo, path, digest, size, content_type, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, component, q.Repo.Name, path, up.Digest, up.Size, ct, h.nowUnix()))
		stmts = append(stmts, h.Blobs.Done(up)...)
		stmts = append(stmts, format.ClaimStmts(q.Set, q.Repo.Name, h.name(path))...) // a name that is published is claimed in the groups that hold this repository (R3)
		ev := h.event(q, "put", path, audit.OK, fmt.Sprintf("%s %d", up.Digest, up.Size))
		stmts = append(stmts, h.Audit.Stmt(ev))
		if _, err := h.DB.Exec(ctx, stmts...); err != nil {
			if why, refused := meta.RefusedBy(err); refused {
				switch why {
				case "changed":
					format.Pause(ctx, try) // another request changed the path between the look and the commit: look again
					continue
				case "quota_bytes", "quota_components":
					_ = h.Blobs.Abandon(ctx, up)
					h.overQuota(w, q, path, why)
					return
				}
			}
			h.unavailable(w, err) // the upload stays: it is swept if nobody finishes it
			return
		}
		h.Audit.Emit(ev)
		h.count(h.Writes, q.Repo.Name)
		code := http.StatusCreated
		if old != nil {
			code = http.StatusOK
		}
		h.done(w, code, path, up)
		return
	}
	_ = h.Blobs.Abandon(ctx, up)
	fail(w, http.StatusConflict, "the path kept changing under this write: try again", nil)
}

func (h *Raw) nowUnix() int64 {
	if h.Now != nil {
		return h.Now().Unix()
	}
	return time.Now().Unix()
}

func (h *Raw) setActive() {
	if h.Active != nil {
		h.Active.Set(int64(h.Downloads.InUse()))
	}
}

func (h *Raw) done(w http.ResponseWriter, code int, path string, up *blob.Upload) {
	w.Header().Set("ETag", `"`+up.Digest+`"`)
	w.Header().Set("X-Checksum-Sha256", up.Digest)
	reply(w, code, map[string]any{"path": path, "sha256": up.Digest, "size": up.Size})
}

func (h *Raw) conflict(w http.ResponseWriter, q format.Request, path, have string) {
	h.count(h.Refused, "conflict")
	h.Audit.Record(context.Background(), h.event(q, "put", path, audit.Refused, "the path is there with other bytes and the repository does not overwrite"))
	fail(w, http.StatusConflict, "the path is there with other bytes; a file is not overwritten in this repository", map[string]string{"sha256": have})
}

func (h *Raw) overQuota(w http.ResponseWriter, q format.Request, path, which string) {
	h.count(h.Refused, "quota")
	h.Audit.Record(context.Background(), h.event(q, "put", path, audit.Refused, which))
	fail(w, http.StatusRequestEntityTooLarge, "the repository is over its quota", map[string]any{"quota": which, "quota_bytes": q.Repo.QuotaBytes, "quota_components": q.Repo.QuotaComponents})
}

func (h *Raw) unavailable(w http.ResponseWriter, err error) {
	var u *meta.Unavailable
	if errors.As(err, &u) || errors.Is(err, context.Canceled) {
		fail(w, http.StatusServiceUnavailable, "the database or the store is not available: try again", nil)
		return
	}
	fail(w, http.StatusBadGateway, "the store failed: try again", nil)
}

func (h *Raw) get(w http.ResponseWriter, r *http.Request, q format.Request) {
	path := q.Path
	if err := CheckPath(path); err != nil {
		fail(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if !q.Authorize(w, auth.Read, path) {
		return
	}
	a, err := h.find(r.Context(), q.Repo.Name, path)
	if err != nil {
		h.unavailable(w, err)
		return
	}
	if a == nil {
		fail(w, http.StatusNotFound, "no such file", nil)
		return
	}
	etag := `"` + a.digest + `"`
	hd := w.Header()
	hd.Set("ETag", etag)
	hd.Set("X-Checksum-Sha256", a.digest)
	hd.Set("Accept-Ranges", "bytes")
	ct := a.contentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	hd.Set("Content-Type", ct)
	// Files that users upload are never run as a page by a browser (docs/spec.md, O8).
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "sandbox")
	for k, v := range a.props {
		hd.Set(propHeader+k, v)
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && (inm == "*" || strings.Contains(inm, etag)) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	from, to, partial, ok := format.ParseRange(r.Header.Get("Range"), a.size)
	if !ok {
		hd.Set("Content-Range", fmt.Sprintf("bytes */%d", a.size))
		fail(w, http.StatusRequestedRangeNotSatisfiable, "the range cannot be satisfied", nil)
		return
	}
	h.LastRead.Add(a.component)
	length := to - from + 1
	if a.size == 0 {
		length = 0
	}
	hd.Set("Content-Length", strconv.FormatInt(length, 10))
	code := http.StatusOK
	if partial {
		hd.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, a.size))
		code = http.StatusPartialContent
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(code)
		return
	}
	h.count(h.Reads, q.Repo.Name)
	if length == 0 {
		w.WriteHeader(code)
		return
	}
	release, ok := h.Downloads.Acquire(r.Context())
	if !ok {
		h.count(h.Refused, "busy")
		hd.Del("Content-Length")
		hd.Set("Retry-After", "5")
		fail(w, http.StatusServiceUnavailable, "too many downloads are going on: try again", nil)
		return
	}
	h.setActive()
	defer func() { release(); h.setActive() }()
	rc, _, err := h.Blobs.Open(r.Context(), a.digest, from, to)
	if err != nil {
		hd.Del("Content-Length")
		if errors.Is(err, blob.ErrNotFound) {
			fail(w, http.StatusInternalServerError, "the bytes of this file are missing: run `godepot reconcile`", nil)
			return
		}
		h.unavailable(w, err)
		return
	}
	defer rc.Close()
	w.WriteHeader(code)
	_, _ = format.CopyPaced(w, rc, h.Rate) // a client that goes away, or is too slow, ends the copy; the status has been sent
}

func (h *Raw) delete(w http.ResponseWriter, r *http.Request, q format.Request) {
	path := q.Path
	if err := CheckPath(path); err != nil {
		fail(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if !q.Authorize(w, auth.Delete, path) {
		return
	}
	ctx := r.Context()
	for try := 0; try < maxTries; try++ {
		a, err := h.find(ctx, q.Repo.Name, path)
		if err != nil {
			h.unavailable(w, err)
			return
		}
		if a == nil {
			fail(w, http.StatusNotFound, "no such file", nil)
			return
		}
		if h.Trash != nil && h.Trash.Days > 0 {
			// To the trash, where it can be put back for a time (C3); the item is called by its path.
			_, err = h.Trash.Move(ctx, q.Repo.Name, []string{a.id}, path, "", q.Actor(), "delete")
		} else {
			ev := h.event(q, "delete", path, audit.OK, a.digest)
			stmts := []meta.Stmt{meta.Guard("changed", `SELECT 1 FROM assets WHERE id = ?`, a.id)}
			stmts = append(stmts, h.Blobs.Drop("asset:"+a.id)...)
			stmts = append(stmts,
				meta.S(`DELETE FROM assets WHERE id = ?`, a.id),
				meta.S(`DELETE FROM components WHERE id = ?`, a.component),
				h.Audit.Stmt(ev))
			if _, err = h.DB.Exec(ctx, stmts...); err == nil {
				h.Audit.Emit(ev)
			}
		}
		if err != nil {
			if why, refused := meta.RefusedBy(err); refused && why == "changed" {
				format.Pause(ctx, try)
				continue
			}
			h.unavailable(w, err)
			return
		}
		h.count(h.Deletes, q.Repo.Name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	fail(w, http.StatusConflict, "the path kept changing under this delete: try again", nil)
}

// list answers a GET of a directory: the files below it, in order of path, at most limit (default 1000, at most 10000), after a path. property=k:v keeps the files whose property k is v.
func (h *Raw) list(w http.ResponseWriter, r *http.Request, q format.Request) {
	prefix := q.Path
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if !q.Authorize(w, auth.Read, prefix) {
		return
	}
	qs := r.URL.Query()
	limit := 1000
	if s := qs.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 10000 {
			fail(w, http.StatusBadRequest, "limit is a number from 1 to 10000", nil)
			return
		}
		limit = n
	}
	sql := `SELECT a.path, a.size, a.digest, a.content_type, a.created_at, c.props FROM assets a JOIN components c ON c.id = a.component_id
		WHERE a.repo = ? AND substr(a.path, 1, ?) = ? AND a.path > ?`
	args := []any{q.Repo.Name, len(prefix), prefix, qs.Get("after")}
	if p := qs.Get("property"); p != "" {
		k, v, ok := strings.Cut(p, ":")
		if !ok || !propKeyRE.MatchString(k) {
			fail(w, http.StatusBadRequest, "property is name:value", nil)
			return
		}
		sql += ` AND json_extract(c.props, ?) = ?`
		args = append(args, `$."`+k+`"`, v)
	}
	sql += ` ORDER BY a.path LIMIT ?`
	args = append(args, limit+1)
	rows, err := h.DB.Query(r.Context(), meta.S(sql, args...))
	if err != nil {
		h.unavailable(w, err)
		return
	}
	type item struct {
		Path        string            `json:"path"`
		Size        int64             `json:"size"`
		SHA256      string            `json:"sha256"`
		ContentType string            `json:"content_type,omitempty"`
		Created     int64             `json:"created_at"`
		Properties  map[string]string `json:"properties,omitempty"`
	}
	out := struct {
		Items []item `json:"items"`
		Next  string `json:"next,omitempty"`
	}{Items: []item{}}
	for i, row := range rows {
		if i == limit {
			out.Next = out.Items[len(out.Items)-1].Path
			break
		}
		it := item{Path: meta.Str(row[0]), Size: meta.Int(row[1]), SHA256: meta.Str(row[2]), ContentType: meta.Str(row[3]), Created: meta.Int(row[4])}
		_ = json.Unmarshal([]byte(meta.Str(row[5])), &it.Properties)
		if len(it.Properties) == 0 {
			it.Properties = nil
		}
		out.Items = append(out.Items, it)
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	reply(w, http.StatusOK, out)
}

// Has says whether the repository holds the name of the path (or, for a proxy, can get it): what a group asks of each member.
func (h *Raw) Has(ctx context.Context, l *settings.Repo, path string) (bool, error) {
	return h.has(ctx, l, path)
}

// Lookup says what digest the file at the path has in the repository ("", false: there is no such file). For a format built on raw.
func (h *Raw) Lookup(ctx context.Context, repo, path string) (digest string, ok bool, err error) {
	a, err := h.find(ctx, repo, path)
	if err != nil || a == nil {
		return "", false, err
	}
	return a.digest, true, nil
}
