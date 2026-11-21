package meta

import (
	"context"
	"fmt"
)

// The schema is a list of steps. A step is applied once, in order, together with the number it brings the schema to; a program refuses a database whose number is above its own (docs/spec.md, D4).
// A step is never edited after a release: a change is a new step.
var steps = [][]string{
	1: {
		// guard stays empty for ever: an INSERT of 0 into it fails its CHECK, which undoes the transaction the INSERT is in (Guard, Unless).
		`CREATE TABLE guard (ok INTEGER NOT NULL CHECK (ok = 1))`,

		// A blob: bytes named by their SHA-256. key is where the bytes are in the store; it differs for every life of the digest (a blob that was collected and came back has another key),
		// so that a late delete of an old life cannot remove a new one. state is 'incoming' (the row is there, the bytes may not be) or 'live'.
		// unref_since is NULL exactly while a row of refs names the digest; otherwise it is the time since when none does.
		`CREATE TABLE blobs (
			digest TEXT PRIMARY KEY,
			size INTEGER NOT NULL,
			key TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('incoming', 'live')),
			unref_since INTEGER
		) WITHOUT ROWID`,
		`CREATE INDEX blobs_unref ON blobs (unref_since) WHERE unref_since IS NOT NULL`,

		// A reference: owner needs digest. An owner is 'upload:<id>', 'asset:<id>', 'publication:<id>'.
		`CREATE TABLE refs (
			owner TEXT NOT NULL,
			digest TEXT NOT NULL,
			PRIMARY KEY (owner, digest)
		) WITHOUT ROWID`,
		`CREATE INDEX refs_digest ON refs (digest)`,

		// An upload in progress: the bytes are being written to key ('uploads/<id>') or have been. The row goes when the caller commits what it uploaded, or when the sweep finds it too old.
		`CREATE TABLE uploads (
			id TEXT PRIMARY KEY,
			key TEXT NOT NULL,
			started INTEGER NOT NULL
		) WITHOUT ROWID`,

		// A grave: a key of the store that is to be deleted. It is written in the transaction that makes the key unneeded, and removed after the store has deleted the key.
		`CREATE TABLE graves (
			key TEXT PRIMARY KEY,
			since INTEGER NOT NULL
		) WITHOUT ROWID`,
	},
	// 2: who does the background work (internal/lease).
	2: {
		`CREATE TABLE lease (
			name TEXT PRIMARY KEY,
			holder TEXT NOT NULL,
			until INTEGER NOT NULL
		) WITHOUT ROWID`,
	},
	// 3: accounts and tokens (internal/auth), components and assets (the formats), the audit log (internal/audit).
	3: {
		// An account is a user or a robot. It has no password: it gets in with tokens. admin is every action on every repository (a token still has its own scope).
		`CREATE TABLE accounts (
			name TEXT PRIMARY KEY,
			kind TEXT NOT NULL CHECK (kind IN ('user', 'robot')),
			admin INTEGER NOT NULL DEFAULT 0,
			disabled INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		) WITHOUT ROWID`,
		// A token: id is public (it is in the token and in the audit log); only the SHA-256 of the secret part is kept. scope is a JSON list of {repositories, paths, actions}.
		`CREATE TABLE tokens (
			id TEXT PRIMARY KEY,
			account TEXT NOT NULL,
			digest TEXT NOT NULL,
			scope TEXT NOT NULL,
			note TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			revoked_at INTEGER
		) WITHOUT ROWID`,
		`CREATE INDEX tokens_account ON tokens (account)`,
		// A component is one version of one package; for the format raw, one file: name is its path and version is empty. props is a JSON object of text values.
		`CREATE TABLE components (
			id TEXT PRIMARY KEY,
			repo TEXT NOT NULL,
			namespace TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			version TEXT NOT NULL DEFAULT '',
			props TEXT NOT NULL DEFAULT '{}',
			created_by TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			UNIQUE (repo, namespace, name, version)
		) WITHOUT ROWID`,
		// An asset names a blob. Its id is the owner of the reference ('asset:<id>'): a new id for every write, so that an overwrite lets go of the old bytes and takes the new in one list.
		`CREATE TABLE assets (
			id TEXT PRIMARY KEY,
			component_id TEXT NOT NULL,
			repo TEXT NOT NULL,
			path TEXT NOT NULL,
			digest TEXT NOT NULL,
			size INTEGER NOT NULL,
			content_type TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			UNIQUE (repo, path)
		) WITHOUT ROWID`,
		`CREATE INDEX assets_component ON assets (component_id)`,
		// The audit log: one row for every write, delete, change of accounts and tokens, and refusal.
		`CREATE TABLE audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			at INTEGER NOT NULL,
			actor TEXT NOT NULL,
			token TEXT NOT NULL DEFAULT '',
			remote TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			repo TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			result TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX audit_at ON audit (at)`,
	},
	// 4: the format oci (internal/oci). A name is "<repository>/<image>"; a blob, a manifest and a tag belong to a name. A blob of a name holds the blob of the layer under the owner 'ociblob:<repo>/<image>@<digest>'.
	4: {
		// The blobs a name has: pushed to it, or mounted into it. Only a name that has a blob may read it.
		`CREATE TABLE oci_blobs (
			repo TEXT NOT NULL,
			image TEXT NOT NULL,
			digest TEXT NOT NULL,
			size INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (repo, image, digest)
		) WITHOUT ROWID`,
		// A manifest of a name. body holds it up to 256 KiB; a larger one is a blob that the owner 'ocimanifest:<repo>/<image>@<digest>' holds (body is NULL). subject is the digest a referrer names, or ''.
		`CREATE TABLE oci_manifests (
			repo TEXT NOT NULL,
			image TEXT NOT NULL,
			digest TEXT NOT NULL,
			media_type TEXT NOT NULL,
			size INTEGER NOT NULL,
			body TEXT,
			subject TEXT NOT NULL DEFAULT '',
			artifact_type TEXT NOT NULL DEFAULT '',
			annotations TEXT NOT NULL DEFAULT '',
			created_by TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (repo, image, digest)
		) WITHOUT ROWID`,
		`CREATE INDEX oci_manifests_subject ON oci_manifests (repo, image, subject) WHERE subject <> ''`,
		`CREATE TABLE oci_tags (
			repo TEXT NOT NULL,
			image TEXT NOT NULL,
			tag TEXT NOT NULL,
			digest TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (repo, image, tag)
		) WITHOUT ROWID`,
		`CREATE INDEX oci_tags_digest ON oci_tags (repo, image, digest)`,
		// What a manifest needs: kind 'child' (a manifest an index names) or 'blob' (its config and layers). Nothing that a link names is removed while the manifest stays (docs/spec.md, C5).
		`CREATE TABLE oci_links (
			repo TEXT NOT NULL,
			image TEXT NOT NULL,
			from_digest TEXT NOT NULL,
			to_digest TEXT NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('child', 'blob')),
			PRIMARY KEY (repo, image, from_digest, to_digest, kind)
		) WITHOUT ROWID`,
		`CREATE INDEX oci_links_to ON oci_links (repo, image, to_digest)`,
		// An upload in parts, from POST to the PUT that ends it. Each part is a blob held under the owner 'ociupload:<id>'.
		`CREATE TABLE oci_uploads (
			id TEXT PRIMARY KEY,
			repo TEXT NOT NULL,
			image TEXT NOT NULL,
			started INTEGER NOT NULL,
			size INTEGER NOT NULL DEFAULT 0,
			parts INTEGER NOT NULL DEFAULT 0
		) WITHOUT ROWID`,
		`CREATE TABLE oci_parts (
			upload_id TEXT NOT NULL,
			n INTEGER NOT NULL,
			digest TEXT NOT NULL,
			size INTEGER NOT NULL,
			PRIMARY KEY (upload_id, n)
		) WITHOUT ROWID`,
	},
	// 5: proxy and group (internal/upstream, the formats). What a proxy knows of a file it fetched (its validator and when it last asked), the names that hosted repositories have published and so claim
	// in the groups that hold them (docs/spec.md, R3), the names an upstream said it lacks (P4), and the pause after an upstream's errors (P4).
	5: {
		`ALTER TABLE assets ADD COLUMN etag TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE assets ADD COLUMN checked_at INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE claims (
			grp TEXT NOT NULL,
			name TEXT NOT NULL,
			PRIMARY KEY (grp, name)
		) WITHOUT ROWID`,
		`CREATE TABLE upstream_misses (
			repo TEXT NOT NULL,
			name TEXT NOT NULL,
			until INTEGER NOT NULL,
			PRIMARY KEY (repo, name)
		) WITHOUT ROWID`,
		`CREATE TABLE upstream_state (
			repo TEXT PRIMARY KEY,
			fails INTEGER NOT NULL,
			pause_until INTEGER NOT NULL,
			last_error TEXT NOT NULL DEFAULT ''
		) WITHOUT ROWID`,
	},
	// 6: cleanup and the trash (internal/trash, internal/cleanup; docs/spec.md, C1 to C4). last_read is the day (days since 1970) a component was last read, kept to the day and written in batches (C4).
	// A removed thing is moved to the trash, whole: payload holds its rows as JSON, and the owner 'trash:<id>' holds its blobs, so nothing is freed until the trash is emptied. A report is the last
	// that a policy made for a repository.
	6: {
		`ALTER TABLE components ADD COLUMN last_read INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE trash (
			id TEXT PRIMARY KEY,
			repo TEXT NOT NULL,
			name TEXT NOT NULL,
			version TEXT NOT NULL,
			deleted_at INTEGER NOT NULL,
			deleted_by TEXT NOT NULL,
			reason TEXT NOT NULL,
			bytes INTEGER NOT NULL,
			assets INTEGER NOT NULL,
			payload TEXT NOT NULL
		) WITHOUT ROWID`,
		`CREATE INDEX trash_repo ON trash (repo, deleted_at)`,
		`CREATE TABLE cleanup_reports (
			repo TEXT PRIMARY KEY,
			at INTEGER NOT NULL,
			enforced INTEGER NOT NULL,
			body TEXT NOT NULL
		) WITHOUT ROWID`,
	},
}

// Version is the number of the schema this program writes.
var Version = len(steps) - 1

// Migrate brings the database to Version. It is safe to call from several replicas at once: a step that another replica has applied is refused by the unique number and is then skipped.
func Migrate(ctx context.Context, db DB) error {
	if _, err := db.Exec(ctx, S(`CREATE TABLE IF NOT EXISTS schema_steps (n INTEGER PRIMARY KEY, at INTEGER NOT NULL DEFAULT (unixepoch()))`)); err != nil {
		return err
	}
	rows, err := db.Query(ctx, S(`SELECT COALESCE(MAX(n), 0) FROM schema_steps`))
	if err != nil {
		return err
	}
	have := int(Int(rows[0][0]))
	if have > Version {
		return fmt.Errorf("the database has schema %d and this program knows %d: use the newer program", have, Version)
	}
	for n := have + 1; n <= Version; n++ {
		list := []Stmt{S(`INSERT INTO schema_steps (n) VALUES (?)`, n)}
		for _, sql := range steps[n] {
			list = append(list, S(sql))
		}
		if _, err := db.Exec(ctx, list...); err != nil {
			if _, refused := RefusedBy(err); refused {
				continue // another replica applied this step
			}
			return fmt.Errorf("schema step %d: %w", n, err)
		}
	}
	return nil
}
