package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
)

// The media types of manifests that are taken in: the OCI ones and the two of Docker (schema 2).
const (
	mtOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mtOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mtDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mtDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

func isIndex(mt string) bool { return mt == mtOCIIndex || mt == mtDockerList }
func isImage(mt string) bool { return mt == mtOCIManifest || mt == mtDockerManifest }

type descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// manifest is what godepot reads of a manifest; the bytes are kept as they came.
type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        *descriptor       `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Manifests     []descriptor      `json:"manifests"`
	Subject       *descriptor       `json:"subject"`
	Annotations   map[string]string `json:"annotations"`
}

func (h *OCI) manifest(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.getManifest(w, r, q, rt)
	case http.MethodPut:
		h.putManifest(w, r, q, rt)
	case http.MethodDelete:
		h.deleteManifest(w, r, q, rt)
	default:
		h.method(w, "GET, HEAD, PUT, DELETE")
	}
}

// reference says what a reference is: a digest (hex), or a tag. A reference that is neither is a 400 when it is written to; when it is read or deleted nothing can be there under it, which is a 404.
func reference(w http.ResponseWriter, h *OCI, ref string, write bool) (digest, tag string, ok bool) {
	bad := func(code, msg string) (string, string, bool) {
		if write {
			h.fail(w, http.StatusBadRequest, code, msg, nil)
		} else {
			h.fail(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "this name has no manifest under such a reference", nil)
		}
		return "", "", false
	}
	if strings.Contains(ref, ":") {
		d, valid := ValidDigest(ref)
		if !valid {
			return bad("DIGEST_INVALID", "the digest is not sha256: and 64 hex digits")
		}
		return d, "", true
	}
	if !tagRE.MatchString(ref) {
		return bad("TAG_INVALID", "a tag is up to 128 of letters, digits, '_', '.', '-', and does not begin with '.' or '-'")
	}
	return "", ref, true
}

func (h *OCI) getManifest(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	digest, tag, ok := reference(w, h, rt.ref, false)
	if !ok {
		return
	}
	if !q.Authorize(w, auth.Read, rt.image) {
		return
	}
	ctx := r.Context()
	if tag != "" {
		rows, err := h.DB.Query(ctx, meta.S(`SELECT digest FROM oci_tags WHERE repo = ? AND image = ? AND tag = ?`, q.Repo.Name, rt.image, tag))
		if err != nil {
			h.unavailable(w, err)
			return
		}
		if len(rows) == 0 {
			h.fail(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "this name has no such tag", map[string]string{"tag": tag})
			return
		}
		digest = meta.Str(rows[0][0])
	}
	rows, err := h.DB.Query(ctx, meta.S(`SELECT media_type, size, body FROM oci_manifests WHERE repo = ? AND image = ? AND digest = ?`, q.Repo.Name, rt.image, digest))
	if err != nil {
		h.unavailable(w, err)
		return
	}
	if len(rows) == 0 {
		h.fail(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "this name has no manifest with this digest", map[string]string{"digest": "sha256:" + digest})
		return
	}
	mt, size := meta.Str(rows[0][0]), meta.Int(rows[0][1])
	hd := w.Header()
	hd.Set("Content-Type", mt)
	hd.Set("Docker-Content-Digest", "sha256:"+digest)
	hd.Set("ETag", `"sha256:`+digest+`"`)
	hd.Set("Content-Length", itoa(size))
	hd.Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	h.count(h.Reads, q.Repo.Name)
	if !meta.Null(rows[0][2]) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, meta.Str(rows[0][2]))
		return
	}
	rc, _, err := h.Blobs.Open(ctx, digest, 0, -1)
	if err != nil {
		hd.Del("Content-Length")
		if errors.Is(err, blob.ErrNotFound) {
			h.fail(w, http.StatusInternalServerError, "MANIFEST_UNKNOWN", "the bytes of this manifest are missing: run `godepot reconcile`", nil)
			return
		}
		h.unavailable(w, err)
		return
	}
	defer rc.Close()
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (h *OCI) putManifest(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	want, tag, ok := reference(w, h, rt.ref, true)
	if !ok {
		return
	}
	if !q.Authorize(w, auth.Write, rt.image) {
		return
	}
	ctx := r.Context()
	body, err := io.ReadAll(io.LimitReader(r.Body, h.maxManifest()+1))
	if err != nil {
		h.fail(w, http.StatusBadRequest, "MANIFEST_INVALID", "the body could not be read", nil)
		return
	}
	if int64(len(body)) > h.maxManifest() {
		h.fail(w, http.StatusRequestEntityTooLarge, "MANIFEST_INVALID", fmt.Sprintf("a manifest is at most %d bytes", h.maxManifest()), nil)
		return
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if want != "" && want != digest {
		h.count(h.Refused, "digest")
		h.Audit.Record(ctx, h.event(q, "put", rt.image, "/manifests/"+rt.ref, audit.Refused, "the bytes do not have the digest of the address"))
		h.fail(w, http.StatusBadRequest, "DIGEST_INVALID", "the manifest does not have the digest of the address", map[string]string{"digest": "sha256:" + digest})
		return
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		h.fail(w, http.StatusBadRequest, "MANIFEST_INVALID", "the Content-Type is the media type of the manifest", nil)
		return
	}
	var m manifest
	if !utf8.Valid(body) || json.Unmarshal(body, &m) != nil {
		h.fail(w, http.StatusBadRequest, "MANIFEST_INVALID", "the manifest is not JSON", nil)
		return
	}
	if m.MediaType != "" && m.MediaType != mt {
		h.fail(w, http.StatusBadRequest, "MANIFEST_INVALID", "the mediaType of the manifest is not its Content-Type", map[string]string{"mediaType": m.MediaType, "contentType": mt})
		return
	}
	blobs, children, why := h.check(mt, &m)
	if why != "" {
		h.fail(w, http.StatusBadRequest, "MANIFEST_INVALID", why, nil)
		return
	}
	if missing, err := h.missing(ctx, q.Repo.Name, rt.image, blobs, children); err != nil {
		h.unavailable(w, err)
		return
	} else if missing != "" {
		h.fail(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "the manifest names a blob or a manifest that this name does not have", map[string]string{"digest": missing})
		return
	}

	// A manifest over 256 KiB is a blob (the row cannot hold it well); a smaller one is the row.
	var up *blob.Upload
	var stored any
	if len(body) > inlineManifest {
		if up, err = h.Blobs.Ingest(ctx, bytes.NewReader(body), digest); err != nil {
			h.unavailable(w, err)
			return
		}
	} else {
		stored = string(body)
	}

	art := m.ArtifactType
	if art == "" && m.Config != nil {
		art = m.Config.MediaType
	}
	subject := ""
	if m.Subject != nil {
		subject, _ = ValidDigest(m.Subject.Digest)
	}
	ann := ""
	if len(m.Annotations) > 0 {
		b, _ := json.Marshal(m.Annotations)
		ann = string(b)
	}
	repo := q.Repo.Name
	var stmts []meta.Stmt
	if len(blobs) > 0 {
		in, args := inList(blobs)
		stmts = append(stmts, meta.Guard("blob_unknown", `SELECT 1 WHERE (SELECT COUNT(*) FROM oci_blobs WHERE repo = ? AND image = ? AND digest IN `+in+`) = ?`, append([]any{repo, rt.image}, append(args, int64(len(blobs)))...)...))
	}
	if len(children) > 0 {
		in, args := inList(children)
		stmts = append(stmts, meta.Guard("blob_unknown", `SELECT 1 WHERE (SELECT COUNT(*) FROM oci_manifests WHERE repo = ? AND image = ? AND digest IN `+in+`) = ?`, append([]any{repo, rt.image}, append(args, int64(len(children)))...)...))
	}
	stmts = append(stmts, format.ClaimStmts(q.Set, repo, rt.image)...) // a name that is published is claimed in the groups that hold this repository (R3)
	stmts = append(stmts, meta.S(`INSERT OR IGNORE INTO oci_manifests (repo, image, digest, media_type, size, body, subject, artifact_type, annotations, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, repo, rt.image, digest, mt, int64(len(body)), stored, subject, art, ann, q.Actor(), h.now()))
	for _, d := range blobs {
		stmts = append(stmts, meta.S(`INSERT OR IGNORE INTO oci_links (repo, image, from_digest, to_digest, kind) VALUES (?, ?, ?, ?, 'blob')`, repo, rt.image, digest, d))
	}
	for _, d := range children {
		stmts = append(stmts, meta.S(`INSERT OR IGNORE INTO oci_links (repo, image, from_digest, to_digest, kind) VALUES (?, ?, ?, ?, 'child')`, repo, rt.image, digest, d))
	}
	if tag != "" {
		if q.Repo.ImmutableTag(tag) {
			stmts = append(stmts, meta.Unless("immutable_tag", `SELECT 1 FROM oci_tags WHERE repo = ? AND image = ? AND tag = ? AND digest <> ?`, repo, rt.image, tag, digest))
		}
		stmts = append(stmts, meta.S(`INSERT INTO oci_tags (repo, image, tag, digest, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (repo, image, tag) DO UPDATE SET digest = excluded.digest, updated_at = excluded.updated_at`, repo, rt.image, tag, digest, h.now()))
	}
	if up != nil {
		stmts = append(stmts, h.Blobs.Hold(manifestOwner(repo, rt.image, digest), digest)...)
		stmts = append(stmts, h.Blobs.Done(up)...)
	}
	ev := h.event(q, "put", rt.image, "/manifests/"+rt.ref, audit.OK, fmt.Sprintf("sha256:%s %d bytes %s", digest, len(body), mt))
	stmts = append(stmts, h.Audit.Stmt(ev))
	if _, err := h.DB.Exec(ctx, stmts...); err != nil {
		if up != nil {
			_ = h.Blobs.Abandon(ctx, up)
		}
		if why, refused := meta.RefusedBy(err); refused {
			switch why {
			case "immutable_tag":
				h.count(h.Refused, "conflict")
				h.Audit.Record(ctx, h.event(q, "put", rt.image, "/manifests/"+rt.ref, audit.Refused, "the tag is immutable and names another manifest"))
				h.fail(w, http.StatusConflict, "DENIED", "this tag is immutable in this repository and names another manifest", map[string]string{"tag": tag})
				return
			case "blob_unknown":
				h.fail(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "the manifest names a blob or a manifest that this name does not have", nil)
				return
			}
		}
		h.unavailable(w, err)
		return
	}
	h.Audit.Emit(ev)
	h.count(h.Writes, q.Repo.Name)
	hd := w.Header()
	hd.Set("Location", h.location(q, rt.image, "/manifests/sha256:"+digest))
	hd.Set("Docker-Content-Digest", "sha256:"+digest)
	if subject != "" {
		hd.Set("OCI-Subject", "sha256:"+subject)
	}
	hd.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// check reads what a manifest names: the blobs (config and layers) and the manifests (children of an index), as distinct hex digests. why is not empty when the manifest cannot be taken.
func (h *OCI) check(mt string, m *manifest) (blobs, children []string, why string) {
	if !isImage(mt) && !isIndex(mt) {
		return nil, nil, "the media type " + mt + " is not one that this registry takes: " + mtOCIManifest + ", " + mtOCIIndex + ", " + mtDockerManifest + ", " + mtDockerList
	}
	if m.SchemaVersion != 2 {
		return nil, nil, "schemaVersion is 2"
	}
	if len(m.Layers)+len(m.Manifests) > maxEntries {
		return nil, nil, fmt.Sprintf("a manifest names at most %d layers or manifests", maxEntries)
	}
	seen := map[string]bool{}
	add := func(list *[]string, d descriptor) string {
		hex, ok := ValidDigest(d.Digest)
		if !ok {
			return "a descriptor has a digest that is not sha256: and 64 hex digits: " + d.Digest
		}
		if d.Size < 0 {
			return "a descriptor has a size below 0"
		}
		if !seen[hex] {
			seen[hex] = true
			*list = append(*list, hex)
		}
		return ""
	}
	if isImage(mt) {
		if m.Config == nil {
			return nil, nil, "an image manifest has a config"
		}
		if why := add(&blobs, *m.Config); why != "" {
			return nil, nil, why
		}
		for _, l := range m.Layers {
			if len(l.Digest) > 0 && (strings.Contains(l.MediaType, "foreign") || strings.Contains(l.MediaType, "nondistributable")) {
				continue // a layer that is kept elsewhere
			}
			if why := add(&blobs, l); why != "" {
				return nil, nil, why
			}
		}
	} else {
		for _, c := range m.Manifests {
			if why := add(&children, c); why != "" {
				return nil, nil, why
			}
		}
	}
	if m.Subject != nil {
		if _, ok := ValidDigest(m.Subject.Digest); !ok {
			return nil, nil, "the subject has a digest that is not sha256: and 64 hex digits"
		}
	}
	return blobs, children, ""
}

// missing names the first blob or manifest that the name does not have ("" when it has all).
func (h *OCI) missing(ctx context.Context, repo, image string, blobs, children []string) (string, error) {
	find := func(table string, list []string) (string, error) {
		if len(list) == 0 {
			return "", nil
		}
		in, args := inList(list)
		rows, err := h.DB.Query(ctx, meta.S(`SELECT digest FROM `+table+` WHERE repo = ? AND image = ? AND digest IN `+in, append([]any{repo, image}, args...)...))
		if err != nil {
			return "", err
		}
		have := map[string]bool{}
		for _, r := range rows {
			have[meta.Str(r[0])] = true
		}
		for _, d := range list {
			if !have[d] {
				return "sha256:" + d, nil
			}
		}
		return "", nil
	}
	if d, err := find("oci_blobs", blobs); err != nil || d != "" {
		return d, err
	}
	return find("oci_manifests", children)
}

func (h *OCI) deleteManifest(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	digest, tag, ok := reference(w, h, rt.ref, false)
	if !ok {
		return
	}
	if !q.Authorize(w, auth.Delete, rt.image) {
		return
	}
	ctx := r.Context()
	repo := q.Repo.Name
	var stmts []meta.Stmt
	detail := ""
	if tag != "" {
		// Removing a tag removes the tag and nothing else (docs/spec.md, C5): the manifest stays, for the cleanup to judge.
		stmts = []meta.Stmt{
			meta.Guard("gone", `SELECT 1 FROM oci_tags WHERE repo = ? AND image = ? AND tag = ?`, repo, rt.image, tag),
			meta.S(`DELETE FROM oci_tags WHERE repo = ? AND image = ? AND tag = ?`, repo, rt.image, tag),
		}
		detail = "tag " + tag
	} else {
		stmts = []meta.Stmt{
			meta.Guard("gone", `SELECT 1 FROM oci_manifests WHERE repo = ? AND image = ? AND digest = ?`, repo, rt.image, digest),
			meta.Unless("in_index", `SELECT 1 FROM oci_links WHERE repo = ? AND image = ? AND to_digest = ? AND kind = 'child'`, repo, rt.image, digest),
			meta.S(`DELETE FROM oci_tags WHERE repo = ? AND image = ? AND digest = ?`, repo, rt.image, digest),
			meta.S(`DELETE FROM oci_links WHERE repo = ? AND image = ? AND from_digest = ?`, repo, rt.image, digest),
			meta.S(`DELETE FROM oci_manifests WHERE repo = ? AND image = ? AND digest = ?`, repo, rt.image, digest),
		}
		stmts = append(stmts, h.Blobs.Drop(manifestOwner(repo, rt.image, digest))...)
		detail = "sha256:" + digest
	}
	ev := h.event(q, "delete", rt.image, "/manifests/"+rt.ref, audit.OK, detail)
	stmts = append(stmts, h.Audit.Stmt(ev))
	if _, err := h.DB.Exec(ctx, stmts...); err != nil {
		if why, refused := meta.RefusedBy(err); refused {
			switch why {
			case "gone":
				h.fail(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "this name has no such manifest or tag", nil)
				return
			case "in_index":
				h.count(h.Refused, "conflict")
				h.Audit.Record(ctx, h.event(q, "delete", rt.image, "/manifests/"+rt.ref, audit.Refused, "an index of this name names the manifest"))
				h.fail(w, http.StatusConflict, "DENIED", "an index of this name lists this manifest: delete the index first", nil)
				return
			}
		}
		h.unavailable(w, err)
		return
	}
	h.Audit.Emit(ev)
	h.count(h.Deletes, repo)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}
