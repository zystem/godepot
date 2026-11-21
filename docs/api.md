# The API, the headers of a client, and the audit log

Reference. Every call of `/api/v1/`, how a client sends its token, and the fields of the audit log. Why it is so: [access.md](access.md). The requests that read and write files are in [raw.md](raw.md). Checked by tests with a real rqlite (`internal/server/api_test.go`) and by the program run against a real blobgate (`cmd/godepot/raw_test.go`).

## The first account

The API needs a token, so the first admin is made with the program, on a machine that can reach the database ([configuration.md](configuration.md)):

```bash
godepot new-account root -admin
godepot new-token root -life 24h -repositories '*' -actions admin
```

The second prints the token on the standard output and the rest (id, end date) on the standard error. Use it as `curl -H "Authorization: Bearer $TOKEN" ...`, or as the password of basic authentication.

## How a client sends it

| Client | How |
|---|---|
| `curl` | `curl -u x:$TOKEN` or `-H "Authorization: Bearer $TOKEN"` |
| a client that wants a user name and a password (`pip`, Maven, `apt`, ...) | any user name, the token as the password |

**Never in the address** (`https://user:token@host/...`, `?token=...`): a query string is not read, and a name and password in an address are turned by the client into the same header. No secret is written to any log or audit line, whichever way it was sent: a test sends one four ways and searches the log and the audit rows for it (`TestTrapT32NoSecretInAnyLog`; checked to fail when a secret is put in a message). The audit line names the token by its id.

## The API

All of `/api/v1/` needs a token (`401` without). Bodies are JSON; an unknown word in a body is `400`.

| Call | Who | Does |
|---|---|---|
| `GET /api/v1/whoami` | any token | the account, whether it is an admin, the token id and its scope |
| `POST /api/v1/accounts` `{"name", "kind": "user"\|"robot", "admin": false}` | admin | makes an account. `409` when the name is taken, `400` for a name that is not allowed |
| `GET /api/v1/accounts` | admin | lists them |
| `POST /api/v1/accounts/<name>/disable`, `.../enable` | admin | `204` |
| `POST /api/v1/tokens` `{"account": "<name>", "scope": [{"repositories": [...], "paths": [...], "actions": [...]}], "life_seconds": 3600, "note": ""}` | a token with the action `admin` on `*` (for its own account; `account` left out means its own), an admin account for any account | makes a token. `201` with `{"token": "...", "info": {...}}`: **the only time the secret is shown**. `400` for a life that is missing or longer than the longest, for no scope, or a bad scope |
| `GET /api/v1/tokens` (`?account=<name>`, `?all=1`) | the same; `account` of another and `all` need an admin account | lists tokens (never their secrets) |
| `DELETE /api/v1/tokens/<id>` | the owner with such a token, or an admin account | revokes it, `204` |
| `POST /api/v1/repositories/<name>/forget` (`?name=<key>`) | admin | for a proxy repository: clears what it remembers as "not found" (all of it, or the one key; the keys are `raw:<path>`, `manifest:<image>@<reference>`, `blob:<image>@<digest>`, `name:<image>`); with no `?name=` it also ends the pause after errors of its upstream. `200` with `{"repository", "forgotten": <rows>}`; `404` when the repository is not a proxy ([proxy.md](proxy.md)) |
| `GET /api/v1/repositories/<name>/cleanup` | admin | the report the repository's policy would make now, changing nothing (`?report=last` is the last report that a run made). `404` for a repository with no policy ([cleanup.md](cleanup.md)) |
| `POST /api/v1/repositories/<name>/cleanup` | admin | run the policy now: it removes only when the repository says `enforce`; the answer is the report |
| `GET /api/v1/trash` (`?repository=<name>`) | admin | what is in the trash: `{"items": [...], "days": 7}`; each item has `id`, `repository`, `name`, `version`, `deleted_at`, `deleted_by`, `reason` (`delete` or `cleanup`), `bytes`, `assets`, `purge_at` |
| `POST /api/v1/trash/<id>/restore` | admin | put the item back whole, with the same ids. `409` when something has been written at one of its paths since; `404` when it is gone |
| `DELETE /api/v1/trash/<id>` | admin | empty the item now: its bytes are let go and are collected after the grace time. `204` |
| `GET /api/v1/audit` (`?action=`, `?actor=`, `?limit=` up to 1000, `?before=<id>`) | admin | the audit log, newest first; `next` is the id to pass as `before` for the next page |
| `GET /api/v1/repositories` | admin | the settings file as the program has it |

"Admin" means an admin account **and** a token whose scope has the action `admin` on repository `*`.

## The audit log

One line in the log (JSON on the standard error, message `audit`) and one row in the database for every **write, delete, refusal** (`401` and `403` included, with the account and the token id when there were any), **account and token change**, and each **`new-account` and `new-token`** (actor `cli`). The fields: `actor`, `token`, `remote` (the address the connection came from; a reverse proxy's `X-Forwarded-For` is **not** trusted), `action`, `repo`, `path`, `result` (`ok`, `refused`), `detail` (why, or the digest and size).

- The row of a write or a delete is written **in the same transaction** as the change. The row of a refusal, of an account and of a token is written after the thing it describes, by itself.
- Reads are not audited (they are counted: [metrics.md](metrics.md)).
- The log is shipped like any log. The rows are not removed by the program.
