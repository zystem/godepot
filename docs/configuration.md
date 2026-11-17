# Configuration

Reference. The environment variables and the commands of the program as they are in this release (2026-11-17). godepot reads no file of settings yet: repositories, upstreams, policies and roles (spec O1) come with the formats. Every change of a variable needs a **restart** of the replica.

## Settings

A secret may be given as `NAME` or as `NAME_FILE` (the path of a file whose contents, without the last line break, are the secret). Setting both is an error. A file is the better way: the value is then not in the environment of the process.

| Variable | Default | Meaning |
|---|---|---|
| `GODEPOT_RQLITE_URL` | **required** | The rqlite that holds the records (`http://host:4001`). The program waits for it up to two minutes at start. |
| `GODEPOT_BLOBGATE_URL` | **required** | The blobgate that keeps the bytes (`http://host:18090`). |
| `GODEPOT_BLOBGATE_TENANT` | **required** | The tenant of blobgate that godepot writes as. One instance of godepot is one tenant; two instances share nothing when they have two. |
| `GODEPOT_BLOBGATE_SECRET`, `GODEPOT_BLOBGATE_SECRET_FILE` | **required** (at least 16 characters) | The secret of that tenant (blobgate's `BLOBGATE_TENANTS`). |
| `GODEPOT_BLOBGATE_STORE` | `_default` | The store of blobgate that holds the bytes (`_default` is the one marked default). It must be set to `dedup: "tenant"`: godepot makes the key of a blob from the bytes of an upload by a link, and `validate` and `serve` stop with a message when the store cannot link. |
| `GODEPOT_PORT` | `18100` | The port of `/healthz` and `/readyz`. Plain HTTP; TLS is not served yet. |
| `GODEPOT_METRICS_PORT` | `0` (off) | A port that serves `/metrics` with no token. **Dangerous** when it can be reached from outside the cluster: it shows how much is collected and whether the replica leads. |
| `GODEPOT_GRACE_SECONDS` | `86400` (24 hours; at least 60) | How long a blob that nothing names is kept before the collector removes it. It must be longer than the longest time between a client learning that a blob exists and committing what names it, and longer than a read of a blob lasts: [blob-layer.md](blob-layer.md). |
| `GODEPOT_UPLOAD_TTL_SECONDS` | `86400` (at least 60) | An upload that has not ended after this is swept: its bytes are removed. It must be longer than the slowest upload. |
| `GODEPOT_COLLECT_SECONDS` | `60` (at least 1) | How often the replica that leads makes a pass of the collector. |
| `GODEPOT_COLLECT_BATCH` | `200` (at least 1) | How many things of each kind (graves, uploads, blobs) a pass handles. A full batch makes the replica go on at once, up to 20 passes in a row. |
| `GODEPOT_LEASE_SECONDS` | `120` (at least 10) | How long the replica that leads holds the lease without renewing it. When it dies, another takes over after this time. It must be at least twice `GODEPOT_COLLECT_SECONDS`, and **much longer than the difference between the clocks of the replicas**: the lease is kept by each replica's own clock. A replica whose clock is far ahead and that dies leaves the lease to run out by the real time, which can be long. |

A wrong value is refused at start with every problem named at once.

## Commands

| Command | What it does |
|---|---|
| `godepot serve` | Waits for the database, brings its schema up to date, tries the store, then serves `/healthz` and `/readyz` and, while it holds the lease, does the background work. `SIGTERM` or `SIGINT` lets go of the lease and ends. |
| `godepot validate` | Checks the settings, that the database answers and that the store can be used (writes a test object of its own and removes it). Prints `settings: ok`, `database: ok`, `store: ok`, or what is wrong. |
| `godepot migrate` | Brings the schema of the database to the one this program writes, and stops. A database whose schema is newer than the program is refused. |
| `godepot check` | Tries the store: a write of unknown length, a link of the same bytes to a second key, a read of a range, a listing, a delete. |
| `godepot reconcile` | Compares the store with the database. Without `-fix` nothing is changed. With `-fix` the keys that nothing names and that are older than the grace time are removed. Live blobs whose bytes are gone are listed (the first 20) and are **not** repaired: that data is lost and only a restore of the store brings it back. |
| `godepot version` | Prints the version. |

`-fix` is for `reconcile` only.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | done |
| 1 | failed: a wrong setting, the database or the store cannot be used, an error |
| 2 | wrong use: no command, an unknown command or flag |
| 3 | `reconcile` found live blobs whose bytes are not in the store |

## What is served

| Path | Answer |
|---|---|
| `GET /healthz` | `ok` while the process runs |
| `GET /readyz` | `{"checks":{"database":"ok"},"status":"ready"}`, or 503 with `"database":"fail"` |
| `GET /metrics` (on `GODEPOT_METRICS_PORT` only) | [metrics.md](metrics.md) |

No format is served in this release.
