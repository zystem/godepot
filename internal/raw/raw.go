// Package raw is the format "raw": files by path, as they are. PUT writes a file, GET and HEAD read it (with ranges), DELETE removes it, and a GET of a directory (a path that ends with "/")
// lists the files below it. It is the smallest format and the first one built end to end: the paths, the immutability (docs/spec.md, R4), the properties (R6), the quota (R7), the audit (A6).
package raw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
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
)

// Raw is the handler of the repositories of the format raw.
type Raw struct {
	Blobs *blob.Layer
	DB    meta.DB
	Audit *audit.Log
	// Reads, Writes and Deletes count the requests per repository; Refused counts by reason. nil: not counted.
	Reads, Writes, Deletes, Refused *metrics.Counter
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

// Serve answers one request.
func (h *Raw) Serve(w http.ResponseWriter, r *http.Request, q format.Request) {
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
	id, component, digest, contentType string
	size, created                      int64
	props                              map[string]string
}

func (h *Raw) find(ctx context.Context, repo, path string) (*asset, error) {
	rows, err := h.DB.Query(ctx, meta.S(`SELECT a.id, a.component_id, a.digest, a.size, a.content_type, a.created_at, c.props
		FROM assets a JOIN components c ON c.id = a.component_id WHERE a.repo = ? AND a.path = ?`, repo, path))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	a := &asset{id: meta.Str(r[0]), component: meta.Str(r[1]), digest: meta.Str(r[2]), size: meta.Int(r[3]), contentType: meta.Str(r[4]), created: meta.Int(r[5]), props: map[string]string{}}
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
		id := blobToken()
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
			component = blobToken()
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
		ev := h.event(q, "put", path, audit.OK, fmt.Sprintf("%s %d", up.Digest, up.Size))
		stmts = append(stmts, h.Audit.Stmt(ev))
		if _, err := h.DB.Exec(ctx, stmts...); err != nil {
			if why, refused := meta.RefusedBy(err); refused {
				switch why {
				case "changed":
					pause(ctx, try) // another request changed the path between the look and the commit: look again
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

// pause waits a little, longer after each try and never the same for two writers, so that writers of one path do not keep colliding.
func pause(ctx context.Context, try int) {
	n := 2 + try
	if n > 20 {
		n = 20
	}
	d := time.Duration(mrand.IntN(n)+1) * time.Millisecond // #nosec G404 -- a back-off, not a secret
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func blobToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // no randomness: nothing safe can be done
	}
	return hex.EncodeToString(b)
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
	from, to, partial, ok := parseRange(r.Header.Get("Range"), a.size)
	if !ok {
		hd.Set("Content-Range", fmt.Sprintf("bytes */%d", a.size))
		fail(w, http.StatusRequestedRangeNotSatisfiable, "the range cannot be satisfied", nil)
		return
	}
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
	rc, _, err := h.Blobs.Open(r.Context(), a.digest, from, to)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			fail(w, http.StatusInternalServerError, "the bytes of this file are missing: run `godepot reconcile`", nil)
			return
		}
		h.unavailable(w, err)
		return
	}
	defer rc.Close()
	w.WriteHeader(code)
	_, _ = io.Copy(w, rc) // a client that goes away ends the copy; the status has been sent
}

// parseRange reads one range of a Range header ("bytes=a-b", "bytes=a-", "bytes=-n") over a body of size bytes. Several ranges, or a header that is not bytes, are answered with the whole body.
// ok is false for a range that lies outside the body.
func parseRange(h string, size int64) (from, to int64, partial, ok bool) {
	if size == 0 {
		return 0, -1, false, h == "" || !strings.HasPrefix(h, "bytes=")
	}
	whole := func() (int64, int64, bool, bool) { return 0, size - 1, false, true }
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return whole()
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return whole()
	}
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, true
	default:
		f, err := strconv.ParseInt(a, 10, 64)
		if err != nil || f < 0 || f >= size {
			return 0, 0, false, false
		}
		t := size - 1
		if b != "" {
			t, err = strconv.ParseInt(b, 10, 64)
			if err != nil || t < f {
				return 0, 0, false, false
			}
			if t > size-1 {
				t = size - 1
			}
		}
		return f, t, true, true
	}
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
		ev := h.event(q, "delete", path, audit.OK, a.digest)
		stmts := []meta.Stmt{meta.Guard("changed", `SELECT 1 FROM assets WHERE id = ?`, a.id)}
		stmts = append(stmts, h.Blobs.Drop("asset:"+a.id)...)
		stmts = append(stmts,
			meta.S(`DELETE FROM assets WHERE id = ?`, a.id),
			meta.S(`DELETE FROM components WHERE id = ?`, a.component),
			h.Audit.Stmt(ev))
		if _, err := h.DB.Exec(ctx, stmts...); err != nil {
			if why, refused := meta.RefusedBy(err); refused && why == "changed" {
				pause(ctx, try)
				continue
			}
			h.unavailable(w, err)
			return
		}
		h.Audit.Emit(ev)
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
