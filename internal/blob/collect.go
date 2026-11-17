package blob

import (
	"context"
	"fmt"
	"strings"

	"github.com/zystem/godepot/internal/meta"
)

// Collected says what one pass of Collect did.
type Collected struct {
	Graves  int // keys deleted that an earlier step had written down
	Uploads int // uploads swept: too old
	Blobs   int // blobs removed: nothing named them for the grace time
}

// Collect does one pass of the background work, at most limit items of each kind: it deletes the keys that have a grave, sweeps the uploads that are too old, and removes the blobs that nothing has
// named for the grace time. It is safe while the service serves, and safe to run on two replicas at once (the lease only saves work). An error stops the pass; the next one goes on.
func (l *Layer) Collect(ctx context.Context, limit int) (Collected, error) {
	var done Collected
	now := l.now()

	// 1. Graves: what a step that died, or a caller's commit, left to delete.
	rows, err := l.DB.Query(ctx, meta.S(`SELECT key FROM graves ORDER BY since LIMIT ?`, limit))
	if err != nil {
		return done, err
	}
	for _, r := range rows {
		if err := l.dig(ctx, meta.Str(r[0])); err != nil {
			return done, err
		}
		done.Graves++
	}

	// 2. Uploads that are too old: their holder is gone. Letting go, the grave of the key and the end of the row are one transaction.
	rows, err = l.DB.Query(ctx, meta.S(`SELECT id, key FROM uploads WHERE started <= ? ORDER BY started LIMIT ?`, now-l.uploadTTL(), limit))
	if err != nil {
		return done, err
	}
	for _, r := range rows {
		up := &Upload{ID: meta.Str(r[0]), key: meta.Str(r[1])}
		if err := l.Abandon(ctx, up); err != nil {
			return done, err
		}
		done.Uploads++
	}

	// 3. Blobs that nothing has named for the grace time. The row goes, and the grave of its key is written, only if at that moment it is still the same life (key), still unnamed since the same
	// time, and no reference names it. A push that took the blob a moment before makes the statement change nothing; a push that comes a moment after finds no blob and sends the bytes.
	rows, err = l.DB.Query(ctx, meta.S(`SELECT digest, key, unref_since FROM blobs WHERE unref_since IS NOT NULL AND unref_since <= ? ORDER BY unref_since LIMIT ?`, now-l.grace(), limit))
	if err != nil {
		return done, err
	}
	for _, r := range rows {
		digest, key, since := meta.Str(r[0]), meta.Str(r[1]), meta.Int(r[2])
		const still = ` FROM blobs WHERE digest = ? AND key = ? AND unref_since = ? AND NOT EXISTS (SELECT 1 FROM refs WHERE refs.digest = blobs.digest)`
		if _, err := l.DB.Exec(ctx,
			meta.S(`INSERT OR IGNORE INTO graves (key, since) SELECT key, ?`+still, now, digest, key, since),
			meta.S(`DELETE`+still, digest, key, since),
		); err != nil {
			return done, err
		}
		// Whether this pass or another removed the row is not asked: a grave that is there is dug, and one that is not there means the blob was taken again.
		l.at("collect:grave")
		g, err := l.DB.Query(ctx, meta.S(`SELECT 1 FROM graves WHERE key = ?`, key))
		if err != nil {
			return done, err
		}
		if len(g) == 0 {
			continue
		}
		if err := l.dig(ctx, key); err != nil {
			return done, err
		}
		done.Blobs++
	}
	return done, nil
}

// Report is what Reconcile found.
type Report struct {
	Missing []string // digests of live blobs whose bytes the store does not have, or has with another size: data is lost, restore the store
	Strays  int      // keys of the store that no row knew, removed
	Checked int      // live blobs looked at
}

// Reconcile compares the store with the database, from the outside: every live blob must have its key in the store, with its size; a key under blobs/ or uploads/ that no row and no grave names,
// and that is older than the grace time, is a stray (a process died between the store and the database, or the database was restored from an older backup) and is removed. With dry, nothing is removed.
// It reads the listing of the store in 256 parts (by the first byte of the digest), so its memory is that of one part.
func (l *Layer) Reconcile(ctx context.Context, dry bool) (Report, error) {
	var rep Report
	old := l.now() - l.grace()
	stray := func(e Entry) error {
		if e.Modified.Unix() > old {
			return nil // young: a write in progress may be about to name it
		}
		rep.Strays++
		if dry {
			return nil
		}
		return l.bury(ctx, e.Key)
	}

	const hex = "0123456789abcdef"
	for _, a := range hex {
		for _, b := range hex {
			part := string(a) + string(b)
			rows, err := l.DB.Query(ctx, meta.S(`SELECT key, size, state, digest FROM blobs WHERE digest >= ? AND digest < ?`, part, part+"g"))
			if err != nil {
				return rep, err
			}
			type row struct {
				size   int64
				live   bool
				digest string
				seen   bool
			}
			known := make(map[string]*row, len(rows))
			for _, r := range rows {
				known[meta.Str(r[0])] = &row{size: meta.Int(r[1]), live: meta.Str(r[2]) == "live", digest: meta.Str(r[3])}
			}
			var unknown []Entry
			if err := l.Store.List(ctx, "blobs/sha256/"+part+"/", func(e Entry) error {
				if k := known[e.Key]; k != nil {
					k.seen = k.size == e.Size
				} else {
					unknown = append(unknown, e)
				}
				return nil
			}); err != nil {
				return rep, fmt.Errorf("listing the store: %w", err)
			}
			for _, k := range known {
				if !k.live {
					continue
				}
				rep.Checked++
				if !k.seen {
					rep.Missing = append(rep.Missing, k.digest)
				}
			}
			if err := l.strays(ctx, unknown, stray); err != nil {
				return rep, err
			}
		}
	}

	var staged []Entry
	if err := l.Store.List(ctx, "uploads/", func(e Entry) error { staged = append(staged, e); return nil }); err != nil {
		return rep, fmt.Errorf("listing the store: %w", err)
	}
	var unknown []Entry
	for _, e := range staged {
		rows, err := l.DB.Query(ctx, meta.S(`SELECT 1 FROM uploads WHERE key = ?`, e.Key))
		if err != nil {
			return rep, err
		}
		if len(rows) == 0 {
			unknown = append(unknown, e)
		}
	}
	return rep, l.strays(ctx, unknown, stray)
}

// strays calls f for the keys that no row knew when the store was listed and that no row and no grave knows now: a blob that was made while the listing ran has a row by now.
func (l *Layer) strays(ctx context.Context, unknown []Entry, f func(Entry) error) error {
	for _, e := range unknown {
		q := `SELECT 1 FROM graves WHERE key = ? UNION ALL SELECT 1 FROM uploads WHERE key = ?`
		args := []any{e.Key, e.Key}
		if strings.HasPrefix(e.Key, "blobs/") {
			q += ` UNION ALL SELECT 1 FROM blobs WHERE key = ?`
			args = append(args, e.Key)
		}
		rows, err := l.DB.Query(ctx, meta.S(q, args...))
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			continue
		}
		if err := f(e); err != nil {
			return err
		}
	}
	return nil
}
