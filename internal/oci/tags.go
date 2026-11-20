package oci

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/zystem/godepot/internal/auth"
	"github.com/zystem/godepot/internal/format"
	"github.com/zystem/godepot/internal/meta"
)

// tags answers GET .../tags/list?n=&last=: the tags of a name in order, a page at a time when n is sent.
func (h *OCI) tags(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	if r.Method != http.MethodGet {
		h.method(w, "GET")
		return
	}
	if !q.Authorize(w, auth.Read, rt.image) {
		return
	}
	ctx := r.Context()
	n := int64(-1)
	if s := r.URL.Query().Get("n"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			h.fail(w, http.StatusBadRequest, "PAGINATION_NUMBER_INVALID", "n is a number from 0", nil)
			return
		}
		n = v
	}
	last := r.URL.Query().Get("last")
	have, err := h.exists(ctx, q.Repo.Name, rt.image)
	if err != nil {
		h.unavailable(w, err)
		return
	}
	if !have {
		h.fail(w, http.StatusNotFound, "NAME_UNKNOWN", "this name has nothing yet", nil)
		return
	}
	list := []string{}
	more := false
	if n != 0 {
		limit := int64(-1)
		if n > 0 {
			limit = n + 1 // one more than a page, to know whether there is a next page
		}
		rows, err := h.DB.Query(ctx, meta.S(`SELECT tag FROM oci_tags WHERE repo = ? AND image = ? AND tag > ? ORDER BY tag LIMIT ?`, q.Repo.Name, rt.image, last, limit))
		if err != nil {
			h.unavailable(w, err)
			return
		}
		for _, row := range rows {
			list = append(list, meta.Str(row[0]))
		}
		if n > 0 && int64(len(list)) > n {
			list, more = list[:n], true
		}
	}
	if more {
		next := url.Values{"n": {strconv.FormatInt(n, 10)}, "last": {list[len(list)-1]}}
		w.Header().Set("Link", `<`+h.location(q, rt.image, "/tags/list?"+next.Encode())+`>; rel="next"`)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": h.address(q) + "/" + rt.image, "tags": list})
}

// referrers answers GET .../referrers/<digest>?artifactType=: an index of the manifests whose subject is the digest. The API is always there, so a client never falls back to the tag schema.
func (h *OCI) referrers(w http.ResponseWriter, r *http.Request, q format.Request, rt route) {
	if r.Method != http.MethodGet {
		h.method(w, "GET")
		return
	}
	hex, ok := ValidDigest(rt.ref)
	if !ok {
		h.fail(w, http.StatusBadRequest, "DIGEST_INVALID", "the digest is not sha256: and 64 hex digits", nil)
		return
	}
	if !q.Authorize(w, auth.Read, rt.image) {
		return
	}
	filter := r.URL.Query().Get("artifactType")
	sql := `SELECT digest, media_type, size, artifact_type, annotations FROM oci_manifests WHERE repo = ? AND image = ? AND subject = ?`
	args := []any{q.Repo.Name, rt.image, hex}
	if filter != "" {
		sql += ` AND artifact_type = ?`
		args = append(args, filter)
	}
	rows, err := h.DB.Query(r.Context(), meta.S(sql+` ORDER BY created_at, digest`, args...))
	if err != nil {
		h.unavailable(w, err)
		return
	}
	list := []descriptor{}
	for _, row := range rows {
		d := descriptor{Digest: "sha256:" + meta.Str(row[0]), MediaType: meta.Str(row[1]), Size: meta.Int(row[2]), ArtifactType: meta.Str(row[3])}
		if a := meta.Str(row[4]); a != "" {
			_ = json.Unmarshal([]byte(a), &d.Annotations)
		}
		list = append(list, d)
	}
	hd := w.Header()
	hd.Set("Content-Type", mtOCIIndex)
	if filter != "" {
		hd.Set("OCI-Filters-Applied", "artifactType")
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "mediaType": mtOCIIndex, "manifests": list})
}
