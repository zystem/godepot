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

All of `/api/v1/` needs a token (`401` without), except `login`, `password` and `oidc/token`, whose credential is in the body. Bodies are JSON; an unknown word in a body is `400`.

| Call | Who | Does |
|---|---|---|
| `GET /api/v1/whoami` | any token | the account, whether it is an admin, the token id and its scope |
| `POST /api/v1/accounts` `{"name", "kind": "user"\|"robot", "admin": false}` | admin | makes an account. `409` when the name is taken, `400` for a name that is not allowed |
| `GET /api/v1/accounts` | admin | lists them |
| `POST /api/v1/accounts/<name>/disable`, `.../enable` | admin | `204` |
| `PUT /api/v1/accounts/<name>/password` `{"password"}` | admin | sets the password of a **user** account (12 to 128 bytes; a robot has none: `400`). `204`. `DELETE` on the same address clears it: the account gets in with tokens only |
| `POST /api/v1/login` `{"account", "password", "life_seconds", "repositories", "paths", "actions", "note"}` | **no token**: the password is the credential | the password for a token (`201`, `{"token", "account", "expires_at"}`, shown once). With only the account and the password the token has `read`, `write`, `delete` and `promote` on every repository for 12 hours; the other words narrow it (a token never has more than its account, and ends within `GODEPOT_TOKEN_MAX_DAYS`). **The action `admin` is refused** (`400`): a password gives no admin token. (Before 2026-11-27 it was taken, and an admin account's password could so reach the admin API; no release has it.) `401` for any login that is not good, in the same words for a wrong password, no such account, a disabled one and one with no password; `429` with `Retry-After` for a locked account or too many failures from one address |
| `POST /api/v1/password` `{"account", "password", "new_password"}` | **no token**: the old password | a user changes their own password. `204`; `401`, `429` as for a login |
| `POST /api/v1/oidc/token` `{"token": "<the job's token>", "account"}` | **no token**: the signed token of a trusted issuer is the credential | a CI job's token for a token of godepot that lives 15 minutes (`201`, `{"token", "account", "expires_at"}`), as the trusts of the settings say ([ci-jobs.md](ci-jobs.md)); `account` chooses among several trusts that fit. `401` for a token that is not accepted (the same words for every reason), `403` when no trust fits or its account cannot be used, `429` after too many refused from one address, `503` when the issuer's keys cannot be read |
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
| `PUT /api/v1/ldap/ca` (the body is PEM text, not JSON) | admin | trust these certificates for the directory, in place of those given before: a `ca.crt`, or a self-signed `server.crt`; the name is not checked when the server shows exactly one of them ([ldap.md](ldap.md)). `200` with `{"certificates": [{"subject", "issuer", "sha256", "not_before", "not_after"}]}`. `400` for anything but certificates (a private key above all), an ended one, or more than 64 KiB |
| `GET /api/v1/ldap/ca` | admin | `{"certificates": [...], "pem", "set_by", "set_at"}`; only `{"certificates": []}` when none was given |
| `DELETE /api/v1/ldap/ca` | admin | forget them, `204` |
| `PUT /api/v1/credentials/upstream/<repository>`, `PUT /api/v1/credentials/ldap` (the body is the password or token itself, not JSON) | admin | keep it, sealed, in place of the one before; line breaks at its end are taken off. `204`. `400` for an empty one, more than one line or more than 8 KiB; `404` when the settings have no such proxy repository or no `ldap` section; `409` when the settings name `password_env` or `token_env` for it; `503` when this replica has an old seal key ([secrets.md](secrets.md)) |
| `DELETE /api/v1/credentials/<the same name>` | admin | forget it, `204` |
| `GET /api/v1/credentials` | admin | `{"items": [{"name", "set_by", "set_at"}]}`; **no call tells a secret** |

"Admin" means an admin account **and** a token whose scope has the action `admin` on repository `*`.

## The audit log

One line in the log (JSON on the standard error, message `audit`) and one row in the database for every **write, delete, refusal** (`401` and `403` included, with the account and the token id when there were any), **account and token change**, each change of the directory's trusted certificates (action `ldap.ca.set`, with their SHA-256), each password kept or forgotten (`credential.set`, `credential.delete`, with the name, never the secret; a refusal of the list is `credential.list`), `seal.reseal` and `seal.reset` of the commands, and each **`new-account` and `new-token`** (actor `cli`). The fields: `actor`, `token`, `remote` (the address the connection came from; a reverse proxy's `X-Forwarded-For` is **not** trusted), `action`, `repo`, `path`, `result` (`ok`, `refused`), `detail` (why, or the digest and size).

- The row of a write or a delete is written **in the same transaction** as the change. The row of a refusal, of an account and of a token is written after the thing it describes, by itself.
- Reads are not audited (they are counted: [metrics.md](metrics.md)).
- The log is shipped like any log. The rows are not removed by the program.

## Tokens of CI jobs

The audit action is `oidc.exchange`: `ok` with the actor the robot and the detail `token <id>, issuer <name>, sub <sub>`, or `refused` with the actor `oidc` and why. The job's token is in no log.

## Passwords

A **user** account may have a password ([access.md](access.md)). It is the password of **basic authentication** (the user name is the account's name; a token is still sent as the password with any user name), so `mvn`, `pip`, `docker login` and `curl -u` can use it, and it is what `POST /api/v1/login` and `npm login` exchange for a token. It gives the account's **data actions** (`read`, `write`, `delete`, `promote`) and **never the admin API**: a call of `/api/v1/` that needs `admin` takes a token made with that scope. Set by an admin with `godepot set-password NAME` (the first line of the standard input) or `PUT /api/v1/accounts/<name>/password`.

- **A password is stored** as PBKDF2-HMAC-SHA256 with a salt of its own and 600 000 rounds (`pbkdf2-sha256$600000$<salt>$<key>`); the rounds are in the string, so the number can rise later without breaking old passwords. A password is checked at most 4 at a time (the rounds are the heavy part); a login that finds them all busy is `429`. A password that was checked is remembered for 5 minutes by its SHA-256, so a client that sends it with every request does not cost the rounds each time; a changed password is another hash and is not matched by the memory.
- **Five failed logins in a row lock the account** for a minute; each failure after that doubles the lock, up to 15 minutes. A locked account is refused even with the right password (`429`, `Retry-After`); its tokens are not touched. The count is in the database, so every replica has it. A success, or a new password, resets it. 30 failures from one address in a minute turn that address away (`429`) before the rounds are spent.
- **What the client is told is the same** for a wrong password, an account that does not exist, a disabled one and one with no password, and the time they take is the same. **One thing does tell:** only a name that exists is locked, so a `429` after five wrong passwords says that it exists. The audit log has the reason (`login`, result `refused`).
- **With a directory** ([ldap.md](ldap.md)) the same calls take its names and passwords; a local account of the same name wins. `PUT .../password` and `POST /api/v1/password` are `400` for an account of the directory. A call that needs the directory when it cannot be asked is `503` with `Retry-After`, and the audit action `directory` says why. (The time of an answer differs between a local account, a name of the directory and an unknown one.)
- **A password is in no log**: not in the audit log, not in the log of the program (a test greps both for the passwords it used).
- The audit actions are `login` (a token made, or a refusal), `password.set`, `password.change`, `password.clear` and `directory` (the directory could not be asked).
