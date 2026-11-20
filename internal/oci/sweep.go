package oci

import (
	"context"

	"github.com/zystem/godepot/internal/meta"
)

// Sweep ends the uploads in parts that took longer than UploadTTL (their client is gone): the references of their parts are let go, to be collected after the grace time. At most limit uploads a
// call; it returns how many it ended. It is safe to run on two replicas at once.
func (h *OCI) Sweep(ctx context.Context, limit int) (int, error) {
	rows, err := h.DB.Query(ctx, meta.S(`SELECT id FROM oci_uploads WHERE started <= ? ORDER BY started LIMIT ?`, h.now()-int64(h.ttl().Seconds()), limit))
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		id := meta.Str(r[0])
		stmts := append(h.Blobs.Drop(uploadOwner(id)), meta.S(`DELETE FROM oci_parts WHERE upload_id = ?`, id), meta.S(`DELETE FROM oci_uploads WHERE id = ?`, id))
		if _, err := h.DB.Exec(ctx, stmts...); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}
