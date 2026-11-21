// Package trash is where what a person or a policy removes goes for a time (docs/spec.md, C3). A removal moves the rows of the removed assets (and of the components left with none) into one row of the
// table trash, and the blobs they held to the owner "trash:<id>", in one transaction: nothing is freed, and the thing can be put back whole until the trash is emptied, by hand or after Days. Reads never
// see the trash: what is in it is gone from every table that a format reads.
package trash

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zystem/godepot/internal/audit"
	"github.com/zystem/godepot/internal/blob"
	"github.com/zystem/godepot/internal/meta"
)

// ErrExists is what a restore returns when something has been written at a path of what is to come back.
var ErrExists = errors.New("something has been written at one of the paths since: remove it first")

// ErrGone is what is returned for an item that is not there (it was restored or emptied).
var ErrGone = errors.New("no such item in the trash")

// Trash is the trash of the repositories.
type Trash struct {
	DB    meta.DB
	Blobs *blob.Layer
	Audit *audit.Log
	Now   func() time.Time
	// Days is how long an item stays (default 7). 0 is "none": a removal frees at once, as it did before the trash.
	Days int
}

func (t *Trash) now() int64 {
	if t.Now != nil {
		return t.Now().Unix()
	}
	return time.Now().Unix()
}

// Item is what the list of the trash says of an item.
type Item struct {
	ID        string `json:"id"`
	Repo      string `json:"repository"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	DeletedAt int64  `json:"deleted_at"`
	DeletedBy string `json:"deleted_by"`
	Reason    string `json:"reason"`
	Bytes     int64  `json:"bytes"`
	Assets    int64  `json:"assets"`
	// PurgeAt is when the item is emptied without being asked (0: Days is 0).
	PurgeAt int64 `json:"purge_at,omitempty"`
}

type assetRow struct {
	ID, Component, Repo, Path, Digest, ContentType, ETag string
	Size, Created, Checked                               int64
}

type componentRow struct {
	ID, Repo, Namespace, Name, Version, Props, CreatedBy string
	Created, LastRead                                    int64
}

type payload struct {
	Assets     []assetRow     `json:"assets"`
	Components []componentRow `json:"components"`
}

func token() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Move takes the assets out of the repository: into the trash, or (Days is 0) out of the world, as a delete always did. name and version say what the item is called in the list; reason says why.
// It is one transaction. An asset that changed since it was looked at (ids are new for every write) refuses the whole move, with meta.RefusedBy word "changed".
func (t *Trash) Move(ctx context.Context, repo string, assetIDs []string, name, version, actor, reason string) (string, error) {
	if len(assetIDs) == 0 {
		return "", errors.New("nothing to move")
	}
	var p payload
	var bytes int64
	for _, id := range assetIDs {
		rows, err := t.DB.Query(ctx, meta.S(`SELECT id, component_id, repo, path, digest, content_type, etag, size, created_at, checked_at FROM assets WHERE id = ? AND repo = ?`, id, repo))
		if err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return "", &meta.Refused{Why: "changed"}
		}
		r := rows[0]
		p.Assets = append(p.Assets, assetRow{meta.Str(r[0]), meta.Str(r[1]), meta.Str(r[2]), meta.Str(r[3]), meta.Str(r[4]), meta.Str(r[5]), meta.Str(r[6]), meta.Int(r[7]), meta.Int(r[8]), meta.Int(r[9])})
		bytes += meta.Int(r[7])
	}
	gone := map[string]bool{}
	for _, a := range p.Assets {
		gone[a.ID] = true
	}
	seen := map[string]bool{}
	for _, a := range p.Assets {
		if seen[a.Component] {
			continue
		}
		seen[a.Component] = true
		rows, err := t.DB.Query(ctx, meta.S(`SELECT id FROM assets WHERE component_id = ?`, a.Component))
		if err != nil {
			return "", err
		}
		left := false
		for _, r := range rows {
			left = left || !gone[meta.Str(r[0])]
		}
		if left {
			continue // the component has other assets: it stays
		}
		crow, err := t.DB.Query(ctx, meta.S(`SELECT id, repo, namespace, name, version, props, created_by, created_at, last_read FROM components WHERE id = ?`, a.Component))
		if err != nil {
			return "", err
		}
		if len(crow) > 0 {
			c := crow[0]
			p.Components = append(p.Components, componentRow{meta.Str(c[0]), meta.Str(c[1]), meta.Str(c[2]), meta.Str(c[3]), meta.Str(c[4]), meta.Str(c[5]), meta.Str(c[6]), meta.Int(c[7]), meta.Int(c[8])})
		}
	}
	id := token()
	var stmts []meta.Stmt
	for _, a := range p.Assets {
		stmts = append(stmts, meta.Guard("changed", `SELECT 1 FROM assets WHERE id = ? AND digest = ?`, a.ID, a.Digest))
	}
	if t.Days > 0 {
		var digests []string
		for _, a := range p.Assets {
			digests = append(digests, a.Digest)
		}
		stmts = append(stmts, t.Blobs.Hold("trash:"+id, digests...)...) // before the owners let go, so no blob is ever unreferenced
	}
	for _, a := range p.Assets {
		stmts = append(stmts, t.Blobs.Drop("asset:"+a.ID)...)
		stmts = append(stmts, meta.S(`DELETE FROM assets WHERE id = ?`, a.ID))
	}
	for _, c := range p.Components {
		stmts = append(stmts, meta.S(`DELETE FROM components WHERE id = ?`, c.ID))
	}
	if t.Days > 0 {
		body, _ := json.Marshal(p)
		stmts = append(stmts, meta.S(`INSERT INTO trash (id, repo, name, version, deleted_at, deleted_by, reason, bytes, assets, payload) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, repo, name, version, t.now(), actor, reason, bytes, len(p.Assets), string(body)))
	}
	ev := audit.Event{Actor: actor, Action: "remove", Repo: repo, Path: name + "@" + version, Result: audit.OK, Detail: fmt.Sprintf("%s: %d assets, %d bytes", reason, len(p.Assets), bytes)}
	stmts = append(stmts, t.Audit.Stmt(ev))
	if _, err := t.DB.Exec(ctx, stmts...); err != nil {
		return "", err
	}
	t.Audit.Emit(ev)
	if t.Days <= 0 {
		return "", nil
	}
	return id, nil
}

// List lists the items of a repository ("" for all), newest first.
func (t *Trash) List(ctx context.Context, repo string) ([]Item, error) {
	q := `SELECT id, repo, name, version, deleted_at, deleted_by, reason, bytes, assets FROM trash`
	var args []any
	if repo != "" {
		q += ` WHERE repo = ?`
		args = append(args, repo)
	}
	rows, err := t.DB.Query(ctx, meta.S(q+` ORDER BY deleted_at DESC, id LIMIT 10000`, args...))
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(rows))
	for _, r := range rows {
		it := Item{ID: meta.Str(r[0]), Repo: meta.Str(r[1]), Name: meta.Str(r[2]), Version: meta.Str(r[3]), DeletedAt: meta.Int(r[4]), DeletedBy: meta.Str(r[5]), Reason: meta.Str(r[6]), Bytes: meta.Int(r[7]), Assets: meta.Int(r[8])}
		if t.Days > 0 {
			it.PurgeAt = it.DeletedAt + int64(t.Days)*86400
		}
		out = append(out, it)
	}
	return out, nil
}

// Restore puts an item back as it was (the same ids, so what a client was told stays true), and takes it out of the trash. A path that has been written meanwhile is ErrExists.
func (t *Trash) Restore(ctx context.Context, id, actor string) (Item, error) {
	rows, err := t.DB.Query(ctx, meta.S(`SELECT repo, name, version, payload, bytes FROM trash WHERE id = ?`, id))
	if err != nil {
		return Item{}, err
	}
	if len(rows) == 0 {
		return Item{}, ErrGone
	}
	repo, name, version := meta.Str(rows[0][0]), meta.Str(rows[0][1]), meta.Str(rows[0][2])
	var p payload
	if err := json.Unmarshal([]byte(meta.Str(rows[0][3])), &p); err != nil {
		return Item{}, fmt.Errorf("the item is damaged: %w", err)
	}
	var stmts []meta.Stmt
	stmts = append(stmts, meta.Guard("gone", `SELECT 1 FROM trash WHERE id = ?`, id))
	for _, c := range p.Components {
		stmts = append(stmts,
			meta.Unless("exists", `SELECT 1 FROM components WHERE repo = ? AND namespace = ? AND name = ? AND version = ?`, c.Repo, c.Namespace, c.Name, c.Version),
			meta.S(`INSERT INTO components (id, repo, namespace, name, version, props, created_by, created_at, last_read) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				c.ID, c.Repo, c.Namespace, c.Name, c.Version, c.Props, c.CreatedBy, c.Created, c.LastRead))
	}
	for _, a := range p.Assets {
		stmts = append(stmts, meta.Unless("exists", `SELECT 1 FROM assets WHERE repo = ? AND path = ?`, a.Repo, a.Path))
		stmts = append(stmts, t.Blobs.Hold("asset:"+a.ID, a.Digest)...)
		stmts = append(stmts, meta.S(`INSERT INTO assets (id, component_id, repo, path, digest, size, content_type, created_at, etag, checked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.Component, a.Repo, a.Path, a.Digest, a.Size, a.ContentType, a.Created, a.ETag, a.Checked))
	}
	stmts = append(stmts, t.Blobs.Drop("trash:"+id)...)
	stmts = append(stmts, meta.S(`DELETE FROM trash WHERE id = ?`, id))
	ev := audit.Event{Actor: actor, Action: "restore", Repo: repo, Path: name + "@" + version, Result: audit.OK, Detail: id}
	stmts = append(stmts, t.Audit.Stmt(ev))
	if _, err := t.DB.Exec(ctx, stmts...); err != nil {
		if why, ok := meta.RefusedBy(err); ok {
			switch why {
			case "exists":
				return Item{}, ErrExists
			case "gone":
				return Item{}, ErrGone
			}
		}
		return Item{}, err
	}
	t.Audit.Emit(ev)
	return Item{ID: id, Repo: repo, Name: name, Version: version, Bytes: meta.Int(rows[0][4])}, nil
}

// Purge empties one item: its blobs are let go (and are collected after their grace time).
func (t *Trash) Purge(ctx context.Context, id, actor string) error {
	rows, err := t.DB.Query(ctx, meta.S(`SELECT repo, name, version FROM trash WHERE id = ?`, id))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrGone
	}
	stmts := []meta.Stmt{meta.Guard("gone", `SELECT 1 FROM trash WHERE id = ?`, id)}
	stmts = append(stmts, t.Blobs.Drop("trash:"+id)...)
	stmts = append(stmts, meta.S(`DELETE FROM trash WHERE id = ?`, id))
	ev := audit.Event{Actor: actor, Action: "purge", Repo: meta.Str(rows[0][0]), Path: meta.Str(rows[0][1]) + "@" + meta.Str(rows[0][2]), Result: audit.OK, Detail: id}
	stmts = append(stmts, t.Audit.Stmt(ev))
	if _, err := t.DB.Exec(ctx, stmts...); err != nil {
		if why, ok := meta.RefusedBy(err); ok && why == "gone" {
			return ErrGone
		}
		return err
	}
	t.Audit.Emit(ev)
	return nil
}

// Sweep empties the items older than Days, up to limit of them. It returns how many.
func (t *Trash) Sweep(ctx context.Context, limit int) (int, error) {
	if t.Days <= 0 {
		return 0, nil
	}
	rows, err := t.DB.Query(ctx, meta.S(`SELECT id FROM trash WHERE deleted_at <= ? ORDER BY deleted_at LIMIT ?`, t.now()-int64(t.Days)*86400, limit))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if err := t.Purge(ctx, meta.Str(r[0]), "trash"); err != nil && !errors.Is(err, ErrGone) {
			return n, err
		}
		n++
	}
	return n, nil
}
