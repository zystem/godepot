package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
)

// linked says the size of a blob that a name has.
func (h *OCI) linked(ctx context.Context, repo, image, hex string) (size int64, ok bool, err error) {
	rows, err := h.DB.Query(ctx, meta.S(`SELECT size FROM oci_blobs WHERE repo = ? AND image = ? AND digest = ?`, repo, image, hex))
	if err != nil || len(rows) == 0 {
		return 0, false, err
	}
	return meta.Int(rows[0][0]), true, nil
}

func (h *OCI) blob(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.getBlob(w, r, q, rt)
	case http.MethodDelete:
		// A blob of a name goes with the cleanup of the repository (docs/spec.md, C1, C5), when nothing names it; a client does not delete one.
		if !q.Authorize(w, auth.Delete, rt.image) {
			return
		}
		h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "a blob is not deleted by a client: it goes with the cleanup of the repository, when no manifest names it", nil)
	default:
		h.method(w, "GET, HEAD, DELETE")
	}
}

func (h *OCI) getBlob(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	hex, ok := ValidDigest(rt.ref)
	if !ok {
		h.fail(w, http.StatusBadRequest, "DIGEST_INVALID", "the digest is not sha256: and 64 hex digits", nil)
		return
	}
	if !q.Authorize(w, auth.Read, rt.image) {
		return
	}
	ctx := r.Context()
	size, found, err := h.linked(ctx, q.Repo.Name, rt.image, hex)
	if err != nil {
		h.unavailable(w, err)
		return
	}
	if !found {
		h.fail(w, http.StatusNotFound, "BLOB_UNKNOWN", "this name has no blob with this digest", map[string]string{"digest": rt.ref})
		return
	}
	hd := w.Header()
	hd.Set("Docker-Content-Digest", rt.ref)
	hd.Set("ETag", `"`+rt.ref+`"`)
	hd.Set("Accept-Ranges", "bytes")
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("X-Content-Type-Options", "nosniff")
	from, to, partial, ok := format.ParseRange(r.Header.Get("Range"), size)
	if !ok {
		hd.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		h.fail(w, http.StatusRequestedRangeNotSatisfiable, "UNSUPPORTED", "the range cannot be satisfied", nil)
		return
	}
	length := to - from + 1
	if size == 0 {
		length = 0
	}
	hd.Set("Content-Length", itoa(length))
	code := http.StatusOK
	if partial {
		hd.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, size))
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
	release, ok := h.Downloads.Acquire(ctx)
	if !ok {
		h.count(h.Refused, "busy")
		hd.Del("Content-Length")
		hd.Set("Retry-After", "5")
		h.fail(w, http.StatusServiceUnavailable, "UNAVAILABLE", "too many downloads are going on: try again", nil)
		return
	}
	h.active()
	defer func() { release(); h.active() }()
	rc, _, err := h.Blobs.Open(ctx, hex, from, to)
	if err != nil {
		hd.Del("Content-Length")
		if errors.Is(err, blob.ErrNotFound) {
			h.fail(w, http.StatusInternalServerError, "BLOB_UNKNOWN", "the bytes of this blob are missing: run `godepot reconcile`", nil)
			return
		}
		h.unavailable(w, err)
		return
	}
	defer rc.Close()
	w.WriteHeader(code)
	_, _ = format.CopyPaced(w, rc, h.Rate) // a client that goes away, or is too slow, ends the copy; the status has been sent
}

// session is an upload in parts.
type session struct {
	id          string
	size, parts int64
}

func (h *OCI) session(ctx context.Context, q format.Request, image, id string) (*session, error) {
	if !uploadRE.MatchString(id) {
		return nil, nil
	}
	rows, err := h.DB.Query(ctx, meta.S(`SELECT size, parts FROM oci_uploads WHERE id = ? AND repo = ? AND image = ?`, id, q.Repo.Name, image))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &session{id: id, size: meta.Int(rows[0][0]), parts: meta.Int(rows[0][1])}, nil
}

func (h *OCI) uploadHeaders(w http.ResponseWriter, q format.Request, image string, s *session) {
	hd := w.Header()
	hd.Set("Location", h.location(q, image, "/blobs/uploads/"+s.id))
	hd.Set("Docker-Upload-UUID", s.id)
	end := s.size - 1
	if end < 0 {
		end = 0
	}
	hd.Set("Range", "0-"+itoa(end))
	hd.Set("Content-Length", "0")
}

// startUpload answers POST .../blobs/uploads/: a mount of a blob, a whole blob in one request, or the start of an upload in parts.
func (h *OCI) startUpload(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	if r.Method != http.MethodPost {
		h.method(w, "POST")
		return
	}
	if !q.Authorize(w, auth.Write, rt.image) {
		return
	}
	ctx := r.Context()
	qs := r.URL.Query()
	if m := qs.Get("mount"); m != "" {
		if hex, ok := ValidDigest(m); ok && h.mount(w, ctx, q, rt.image, hex, qs.Get("from")) {
			return
		}
		// What cannot be mounted is uploaded the usual way, as the specification says.
	}
	if d := qs.Get("digest"); d != "" {
		hex, ok := ValidDigest(d)
		if !ok {
			h.fail(w, http.StatusBadRequest, "DIGEST_INVALID", "the digest is not sha256: and 64 hex digits", nil)
			return
		}
		up, err := h.Blobs.Ingest(ctx, r.Body, hex)
		if !h.ingested(w, q, rt.image, err) {
			return
		}
		h.commitBlob(w, ctx, q, rt.image, hex, up.Size, up, nil, "")
		return
	}
	id := format.Token()
	if _, err := h.DB.Exec(ctx, meta.S(`INSERT INTO oci_uploads (id, repo, image, started) VALUES (?, ?, ?, ?)`, id, q.Repo.Name, rt.image, h.now())); err != nil {
		h.unavailable(w, err)
		return
	}
	s := &session{id: id}
	h.uploadHeaders(w, q, rt.image, s)
	w.WriteHeader(http.StatusAccepted)
}

// ingested answers for an Ingest that failed, and says whether it did not.
func (h *OCI) ingested(w http.ResponseWriter, q format.Request, image string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, blob.ErrDigest):
		h.count(h.Refused, "digest")
		h.Audit.Record(context.Background(), h.event(q, "put", image, "/blobs", audit.Refused, "the bytes do not have the digest that was named"))
		h.fail(w, http.StatusBadRequest, "DIGEST_INVALID", "the bytes do not have the digest that was named", nil)
	default:
		h.unavailable(w, err)
	}
	return false
}

// mount takes a blob of another name of the repository into this one, if the account may read the other. It says whether it answered.
func (h *OCI) mount(w http.ResponseWriter, ctx context.Context, q format.Request, image, hex, from string) bool {
	fromRepo, fromImage, _ := strings.Cut(from, "/")
	if fromRepo != q.Repo.Name || CheckImage(fromRepo, fromImage) != nil || !q.Allowed(auth.Read, fromImage) {
		return false
	}
	size, found, err := h.linked(ctx, fromRepo, fromImage, hex)
	if err != nil || !found {
		return false
	}
	stmts := h.Blobs.Hold(blobOwner(q.Repo.Name, image, hex), hex)
	stmts = append(stmts, meta.S(`INSERT OR IGNORE INTO oci_blobs (repo, image, digest, size, created_at) VALUES (?, ?, ?, ?, ?)`, q.Repo.Name, image, hex, size, h.now()))
	ev := h.event(q, "put", image, "/blobs/sha256:"+hex, audit.OK, "mounted from "+from)
	stmts = append(stmts, h.Audit.Stmt(ev))
	if _, err := h.DB.Exec(ctx, stmts...); err != nil {
		return false // the blob went in the meantime, or the database failed: upload it the usual way
	}
	h.Audit.Emit(ev)
	h.count(h.Writes, q.Repo.Name)
	h.blobCreated(w, q, image, hex)
	return true
}

func (h *OCI) blobCreated(w http.ResponseWriter, q format.Request, image, hex string) {
	hd := w.Header()
	hd.Set("Location", h.location(q, image, "/blobs/sha256:"+hex))
	hd.Set("Docker-Content-Digest", "sha256:"+hex)
	hd.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// commitBlob makes the blob a blob of the name, in one transaction with the end of what held it: the upload up (a blob that was just taken in), the parts of the session. Either may be nil.
func (h *OCI) commitBlob(w http.ResponseWriter, ctx context.Context, q format.Request, image, hex string, size int64, up *blob.Upload, extra []*blob.Upload, sessionID string) {
	var stmts []meta.Stmt
	if sessionID != "" {
		stmts = append(stmts, meta.Guard("upload_gone", `SELECT 1 FROM oci_uploads WHERE id = ?`, sessionID))
	}
	stmts = append(stmts, h.Blobs.Hold(blobOwner(q.Repo.Name, image, hex), hex)...)
	stmts = append(stmts, meta.S(`INSERT OR IGNORE INTO oci_blobs (repo, image, digest, size, created_at) VALUES (?, ?, ?, ?, ?)`, q.Repo.Name, image, hex, size, h.now()))
	if sessionID != "" {
		stmts = append(stmts, h.Blobs.Drop(uploadOwner(sessionID))...)
		stmts = append(stmts, meta.S(`DELETE FROM oci_parts WHERE upload_id = ?`, sessionID), meta.S(`DELETE FROM oci_uploads WHERE id = ?`, sessionID))
	}
	if up != nil {
		stmts = append(stmts, h.Blobs.Done(up)...)
	}
	for _, e := range extra {
		stmts = append(stmts, h.Blobs.Done(e)...)
	}
	ev := h.event(q, "put", image, "/blobs/sha256:"+hex, audit.OK, fmt.Sprintf("%d bytes", size))
	stmts = append(stmts, h.Audit.Stmt(ev))
	if _, err := h.DB.Exec(ctx, stmts...); err != nil {
		if why, refused := meta.RefusedBy(err); refused && (why == "upload_gone" || why == blob.WhyBlobNotLive) {
			h.fail(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "the upload is gone: it took too long, or another request ended it", nil)
			return
		}
		h.unavailable(w, err)
		return
	}
	h.Audit.Emit(ev)
	h.count(h.Writes, q.Repo.Name)
	h.blobCreated(w, q, image, hex)
}

func (h *OCI) upload(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	switch r.Method {
	case http.MethodGet, http.MethodPatch, http.MethodPut, http.MethodDelete:
	default:
		h.method(w, "GET, PATCH, PUT, DELETE")
		return
	}
	action := auth.Write
	if r.Method == http.MethodGet {
		action = auth.Read
	}
	if !q.Authorize(w, action, rt.image) {
		return
	}
	ctx := r.Context()
	s, err := h.session(ctx, q, rt.image, rt.ref)
	if err != nil {
		h.unavailable(w, err)
		return
	}
	if s == nil {
		h.fail(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "no such upload (it may have been swept: an upload may take "+h.ttl().String()+")", nil)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.uploadHeaders(w, q, rt.image, s)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPatch:
		h.patch(w, r, q, rt.image, s)
	case http.MethodPut:
		h.finish(w, r, q, rt.image, s)
	case http.MethodDelete:
		stmts := append(h.Blobs.Drop(uploadOwner(s.id)), meta.S(`DELETE FROM oci_parts WHERE upload_id = ?`, s.id), meta.S(`DELETE FROM oci_uploads WHERE id = ?`, s.id))
		if _, err := h.DB.Exec(ctx, stmts...); err != nil {
			h.unavailable(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// contentRange reads "Content-Range: a-b" (the form of the specification; "bytes a-b" and "bytes a-b/c" are read too).
func contentRange(h string) (start int64, ok bool) {
	h = strings.TrimPrefix(strings.TrimSpace(h), "bytes ")
	a, _, found := strings.Cut(h, "-")
	if !found {
		return 0, false
	}
	n, err := strconv.ParseInt(a, 10, 64)
	return n, err == nil && n >= 0
}

func (h *OCI) patch(w http.ResponseWriter, r *http.Request, q format.Request, image string, s *session) {
	ctx := r.Context()
	if cr := r.Header.Get("Content-Range"); cr != "" {
		if start, ok := contentRange(cr); !ok || start != s.size {
			h.uploadHeaders(w, q, image, s)
			h.fail(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "the range does not start where the upload ends", map[string]int64{"size": s.size})
			return
		}
	}
	if r.ContentLength == 0 {
		h.uploadHeaders(w, q, image, s)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	up, err := h.Blobs.Ingest(ctx, r.Body, "")
	if !h.ingested(w, q, image, err) {
		return
	}
	stmts := []meta.Stmt{meta.Guard("changed", `SELECT 1 FROM oci_uploads WHERE id = ? AND parts = ?`, s.id, s.parts)}
	stmts = append(stmts, h.Blobs.Hold(uploadOwner(s.id), up.Digest)...)
	stmts = append(stmts,
		meta.S(`INSERT INTO oci_parts (upload_id, n, digest, size) VALUES (?, ?, ?, ?)`, s.id, s.parts, up.Digest, up.Size),
		meta.S(`UPDATE oci_uploads SET size = size + ?, parts = parts + 1 WHERE id = ?`, up.Size, s.id))
	stmts = append(stmts, h.Blobs.Done(up)...)
	if _, err := h.DB.Exec(ctx, stmts...); err != nil {
		if why, refused := meta.RefusedBy(err); refused && why == "changed" {
			_ = h.Blobs.Abandon(ctx, up)
			if cur, err := h.session(ctx, q, image, s.id); err == nil && cur != nil {
				s = cur
			}
			h.uploadHeaders(w, q, image, s)
			h.fail(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "another request added to this upload at the same time: send the part again from the size the upload has now", map[string]int64{"size": s.size})
			return
		}
		h.unavailable(w, err)
		return
	}
	s.size += up.Size
	s.parts++
	h.uploadHeaders(w, q, image, s)
	w.WriteHeader(http.StatusAccepted)
}

type part struct {
	digest string
	size   int64
}

// chain reads the blobs one after another, opening each when the one before it is read.
type chain struct {
	ctx  context.Context
	l    *blob.Layer
	list []string
	cur  io.ReadCloser
}

func (c *chain) Read(p []byte) (int, error) {
	for {
		if c.cur == nil {
			if len(c.list) == 0 {
				return 0, io.EOF
			}
			rc, _, err := c.l.Open(c.ctx, c.list[0], 0, -1)
			if err != nil {
				return 0, err
			}
			c.cur, c.list = rc, c.list[1:]
		}
		n, err := c.cur.Read(p)
		if errors.Is(err, io.EOF) {
			_ = c.cur.Close()
			c.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *chain) Close() {
	if c.cur != nil {
		_ = c.cur.Close()
	}
}

// finish answers PUT ...?digest=: the last bytes (if the request has a body), the check of the digest of all of them, and the blob.
func (h *OCI) finish(w http.ResponseWriter, r *http.Request, q format.Request, image string, s *session) {
	ctx := r.Context()
	hex, ok := ValidDigest(r.URL.Query().Get("digest"))
	if !ok {
		h.fail(w, http.StatusBadRequest, "DIGEST_INVALID", "the digest is not sha256: and 64 hex digits", nil)
		return
	}
	rows, err := h.DB.Query(ctx, meta.S(`SELECT digest, size FROM oci_parts WHERE upload_id = ? ORDER BY n`, s.id))
	if err != nil {
		h.unavailable(w, err)
		return
	}
	var parts []part
	for _, row := range rows {
		parts = append(parts, part{meta.Str(row[0]), meta.Int(row[1])})
	}
	hasBody := r.ContentLength != 0

	// Nothing came before: the body is the blob.
	if len(parts) == 0 {
		up, err := h.Blobs.Ingest(ctx, r.Body, hex)
		if !h.ingested(w, q, image, err) {
			return
		}
		h.commitBlob(w, ctx, q, image, hex, up.Size, up, nil, s.id)
		return
	}

	var last *blob.Upload
	if hasBody {
		if last, err = h.Blobs.Ingest(ctx, r.Body, ""); !h.ingested(w, q, image, err) {
			return
		}
		if last.Size == 0 {
			_ = h.Blobs.Abandon(ctx, last)
			last = nil
		}
	}
	abandon := func() {
		if last != nil {
			_ = h.Blobs.Abandon(ctx, last)
		}
	}
	var extra []*blob.Upload
	if last != nil {
		extra = []*blob.Upload{last}
		parts = append(parts, part{last.Digest, last.Size})
	}

	// One piece in all: it has been checked when it was taken in, and needs no second write.
	if len(parts) == 1 {
		if parts[0].digest != hex {
			abandon()
			h.count(h.Refused, "digest")
			h.fail(w, http.StatusBadRequest, "DIGEST_INVALID", "the bytes of the upload do not have the digest that was named", nil)
			return
		}
		h.commitBlob(w, ctx, q, image, hex, parts[0].size, nil, nil, s.id)
		return
	}

	// Several: they are read in order and written as one blob, and the digest of the whole is checked.
	list := make([]string, len(parts))
	for i, p := range parts {
		list[i] = p.digest
	}
	c := &chain{ctx: ctx, l: h.Blobs, list: list}
	up, err := h.Blobs.Ingest(ctx, c, hex)
	c.Close()
	if !h.ingested(w, q, image, err) {
		abandon()
		return
	}
	h.commitBlob(w, ctx, q, image, hex, up.Size, up, extra, s.id)
}
