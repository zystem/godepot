# Configuration

Reference. The environment variables and the commands of the program as they are in this release (2026-11-28). The repositories and who may do what in them are in a file, [settings-file.md](settings-file.md), that the environment names. A proxy repository and the directory may also read the variables that the settings file names (`password_env`, `token_env`: [settings-file.md](settings-file.md)). A change of a variable needs a **restart** of the replica; a change of the settings file needs a restart or a `SIGHUP`.

## Settings

A secret may be given as `NAME` or as `NAME_FILE` (the path of a file whose contents, without the last line break, are the secret). Setting both is an error. A file is the better way: the value is then not in the environment of the process. This holds for `GODEPOT_BLOBGATE_SECRET` and for every name that `password_env` or `token_env` gives in the settings file.

**Changing a secret.** The secret of blobgate is read at start: **restart** the replica. The password of the directory and the credentials of an upstream that are in a file are read again at each use: write the new file (replace it in one step, say with `mv`, so that half a password is never read) and the next request takes it, with no restart. A password that godepot has just checked at the directory is still taken for up to 5 minutes, as before.

| Variable | Default | Meaning |
|---|---|---|
| `GODEPOT_RQLITE_URL` | **required** | The rqlite that holds the records (`http://host:4001`). The program waits for it up to two minutes at start. |
| `GODEPOT_BLOBGATE_URL` | **required** | The blobgate that keeps the bytes (`http://host:18090`). |
| `GODEPOT_BLOBGATE_TENANT` | **required** | The tenant of blobgate that godepot writes as. One instance of godepot is one tenant; two instances share nothing when they have two. |
| `GODEPOT_BLOBGATE_SECRET`, `GODEPOT_BLOBGATE_SECRET_FILE` | **required** (at least 16 characters) | The secret of that tenant (blobgate's `BLOBGATE_TENANTS`). |
| `GODEPOT_BLOBGATE_STORE` | `_default` | The store of blobgate that holds the bytes (`_default` is the one marked default). It must be set to `dedup: "tenant"`: godepot makes the key of a blob from the bytes of an upload by a link, and `validate` and `serve` stop with a message when the store cannot link. |
| `GODEPOT_PORT` | `18100` | The port of everything but `/metrics`: `/healthz`, `/readyz`, the repositories (`/repository/...`, [raw.md](raw.md); `/v2/...`, [oci.md](oci.md)) and the API (`/api/v1/...`, [api.md](api.md)). Plain HTTP; TLS is not served yet, so put it behind a proxy that does, or on a network no one else can reach: **tokens are sent in the clear otherwise**. |
| `GODEPOT_CONFIG_FILE` | none | The path of the settings file ([settings-file.md](settings-file.md)). With none, no repository is served and only the API of accounts answers. A file that is not right stops `serve` and `validate`; on `SIGHUP` it is read again and a file that is not right is refused (the settings in use stay, an error is logged). |
| `GODEPOT_TOKEN_MAX_DAYS` | `90` (at least 1) | The longest life a token may be given. A token always has an end date; asking for more is refused. |
| `GODEPOT_MAX_DOWNLOADS` | `64` (at least 1) | How many blobs and files may be sent to clients at once, for every format. Each holds about 10 MiB of blobgate's memory while it is sent, so the default is about 640 MiB of it ([step0-probes.md](step0-probes.md)). A download over the limit waits up to 5 seconds, then gets `503` with `Retry-After: 5`. |
| `GODEPOT_MIN_DOWNLOAD_RATE` | `8192` (bytes a second; `0` is no least) | A client that takes less than this is ended: each write to it must be taken within ten seconds plus the time its size takes at this speed. **`0` is dangerous**: a client that reads nothing then keeps a place for ever. |
| `GODEPOT_TRASH_DAYS` | `7` (`0` is no trash) | How long what is removed (a delete, a cleanup policy) stays in the trash, where it can be put back whole and still holds its bytes ([cleanup.md](cleanup.md)). **`0` is dangerous**: a removal then frees at once and cannot be undone. |
| `GODEPOT_CLEANUP_SECONDS` | `3600` (at least 60) | How often the replica that leads runs the cleanup policies of the repositories that have one ([cleanup.md](cleanup.md)). A policy that has no `enforce` only writes its report. |
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
| `godepot serve` | Reads the settings file, waits for the database, brings its schema up to date, tries the store, then serves the repositories, the API, `/healthz` and `/readyz` and, while it holds the lease, does the background work. `SIGHUP` reads the settings file again. `SIGTERM` or `SIGINT` lets go of the lease and ends. |
| `godepot validate` | Checks the settings, the settings file (when `GODEPOT_CONFIG_FILE` is set), that the database answers and that the store can be used (writes a test object of its own and removes it). Prints `settings: ok`, then `settings file: ok (N repositories, M grants)` when there is a file, `database: ok`, `store: ok`, or what is wrong. |
| `godepot migrate` | Brings the schema of the database to the one this program writes, and stops. A database whose schema is newer than the program is refused. |
| `godepot check` | Tries the store: a write of unknown length, a link of the same bytes to a second key, a read of a range, a listing, a delete. |
| `godepot reconcile` | Compares the store with the database. Without `-fix` nothing is changed. With `-fix` the keys that nothing names and that are older than the grace time are removed. Live blobs whose bytes are gone are listed (the first 20) and are **not** repaired: that data is lost and only a restore of the store brings it back. |
| `godepot new-account NAME` | Makes an account (after bringing the schema up to date). `-robot` makes a robot, not a user; `-admin` makes an admin account. The name is 1 to 63 of `a-z 0-9 . _ -`, no `$`. An account has no password: it gets in with tokens. This is how the first admin is made, since the API needs a token. |
| `godepot set-password NAME` | Sets the password of a **user** account (a robot has none). The password is the **first line of the standard input**, never an argument (an argument is in the list of processes and in the history of the shell): `printf '%s\n' "$PW" \| godepot set-password alice`. 12 to 128 bytes. Writes the audit log (`password.set`). It prints `the password of alice is set` ([api.md](api.md)). |
| `godepot new-token ACCOUNT` | Makes a token for the account and prints **only the token** on the standard output (the rest, including the end date, goes to the standard error). It is shown once and not kept. `-life` (required, like `720h`, at most `GODEPOT_TOKEN_MAX_DAYS`), `-repositories` and `-actions` (required, comma separated), `-paths` (patterns, none is every path), `-note`. One scope per call; the API makes tokens with more than one. |
| `godepot version` | Prints the version. |

`-fix` is for `reconcile` only. `-robot` and `-admin` are for `new-account`; `-life`, `-repositories`, `-paths`, `-actions` and `-note` are for `new-token`. Both commands write the audit log (the actor is `cli`).

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
| `/repository/<repository>/<path>` | the files of a repository: [raw.md](raw.md) |
| `/api/v1/...` | accounts, tokens, the audit log, the settings: [api.md](api.md) |
| `GET /healthz` | `ok` while the process runs |
| `GET /readyz` | `{"checks":{"database":"ok"},"status":"ready"}`, or 503 with `"database":"fail"` |
| `GET /metrics` (on `GODEPOT_METRICS_PORT` only) | [metrics.md](metrics.md) |

The formats served are `raw`, `oci`, `go`, `pypi`, `npm` and `maven` ([settings-file.md](settings-file.md)).
