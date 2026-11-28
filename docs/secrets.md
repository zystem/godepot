# Passwords of other systems, and the seal key

How-to. godepot shows two kinds of password to others: that of the service account of the directory ([ldap.md](ldap.md)) and those of the upstreams of proxy repositories ([proxy-howto.md](proxy-howto.md)). It keeps them **sealed in the database** (AES-256-GCM, each bound to its name), or takes them from a variable or a file that the settings name. The passwords of people who log in to godepot are a different thing: only their hashes are kept.

## The seal key

- `GODEPOT_SEAL_KEY_FILE` is **required** for `serve` and `validate`: the path of a file with the key (32 random bytes in base64). The key is never in the database.
- When the database has no key yet and the file is not there, `serve` **makes it** (mode 0600) and logs a warning. The directory must be on a disk that lasts: in a container, a volume.
- The database keeps a check value of the key. From then on **only that key is taken**, and no new one is ever made: with no file or another file, `serve` stops and says so.
- **Several replicas need the same file** (in Kubernetes, one Secret mounted in all). Make it before the first start, or copy the one the first replica made: `head -c 32 /dev/urandom | base64 > seal.key && chmod 600 seal.key`.
- **Keep a copy of the file, away from the backup of the database.** A backup of the database alone does not hold the passwords in a form anybody can read.

## Give a password

`$TOKEN` is a token of an admin account with the action `admin` on `*` ([api.md](api.md)). The body is the password itself, from a file or the standard input, never an argument. godepot serves plain HTTP: send it through the proxy that gives TLS, or on a network nobody else is on.

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H "Authorization: Bearer $TOKEN" --data-binary @hub-password localhost:18100/api/v1/credentials/upstream/hub
```

```
204
```

The names are `upstream/<repository>` (the password of its `upstream.username`, or, with no user name, a bearer token) and `ldap`. List what there is (never the secrets):

```bash
curl -s -H "Authorization: Bearer $TOKEN" localhost:18100/api/v1/credentials
```

```
{"items":[{"name":"upstream/hub","set_by":"root","set_at":1795890000}]}
```

(The numbers differ.) A new password replaces the old one; no restart is needed: the replica that took it uses it at the next request, the others within 5 seconds (the directory's: at the next use). `DELETE` on the same address forgets it.

## Or a variable or a file

Name it in the settings with `password_env` or `token_env` ([settings-file.md](settings-file.md)). Then godepot takes the variable `NAME`, or the file that `NAME_FILE` names, and the database is not looked at; a `PUT` for that name is `409`.

## Move to another key

```bash
godepot reseal /var/lib/godepot/seal-2.key
```

It makes the new file when it is not there, seals every password again in one transaction, and prints how many. Then give every replica the new file as `GODEPOT_SEAL_KEY_FILE` and restart it. Until then a replica with the old key cannot read or write passwords. The audit action is `seal.reseal`.

## The key is lost

With a backup of the database and no key, everything of godepot comes back **except the passwords sealed with that key**: nobody can open them. Stop the replicas, then:

```bash
godepot reset-seal-key -yes
```

```
the seal key of the database is reset; passwords removed: 1
  give again: upstream/hub
the next serve takes the key of GODEPOT_SEAL_KEY_FILE, and makes one when the file is not there
```

Start godepot, and give each password named again (above). Repositories, files, accounts, tokens and grants are as they were. Until a password is given again, its proxy repository answers `502` and the directory is one that cannot be asked. The audit action is `seal.reset`. There is no web page for this yet: the pages of godepot change nothing.

## Time to act

A replica whose key is not right (missing, another one, damaged) does not end at once. For **30 seconds** it answers `/healthz` with `ok` and `/readyz` with `503` `{"checks":{"seal_key":"fail"},"status":"not ready"}`, serves nothing else, and tries the key every second. If the key is put right in that time, or reset, it goes on with no restart; else it ends with the message, and whatever runs it starts it again. So, in a container:

```bash
docker exec godepot /godepot reset-seal-key -yes
```

The command needs only the settings of the program, not a running replica: when the 30 seconds are over it can as well be run on its own (`docker run --rm ... godepot reset-seal-key -yes`, with the settings of the replica). A missing `GODEPOT_SEAL_KEY_FILE` (no file named at all) ends the program at once: a reset does not help there.

## Messages

The messages of a missing, other or damaged key end with the way out, so that it is read where it is needed:

> If the key is lost for good: stop the replicas and run `godepot reset-seal-key -yes`. It removes the passwords kept in the database (everything else stays) and prints which to give again; then start, and give them (docs/secrets.md)

| Message | Meaning | What to do |
|---|---|---|
| `GODEPOT_SEAL_KEY_FILE is required: ...` | no file is named | name one, on a disk that lasts |
| `not the seal key of this database: the database has secrets sealed with a key and <file> is not there` | the file is gone, or this replica never had it | put the key back; if it is lost, `reset-seal-key` |
| `not the seal key of this database: <file> holds another key` | another file than the first one | give every replica the same file; if the key is lost, `reset-seal-key` |
| `<file> is not a seal key` | the file is empty or is something else | put the key back; if it is lost, `reset-seal-key` and remove the damaged file |
| `no password is kept for <repository>, which has upstream.username` | nothing was given for this upstream | give it, or name `password_env` |
| `the stored secret "<name>" cannot be opened` | the key was changed while this replica ran, or the row is damaged | restart with the new key; or give the password again |
| `this replica has an old seal key: restart it` (`503`) | `reseal` was run | restart with the new file |

## Checked

- 2026-11-28, the program against a real rqlite and blobgate with a raw proxy of a local server that asks for basic authentication: the key made at the first start; the password given with the API and used; refused with the key file gone; `reset-seal-key -yes`, a new key at the next start, the password given again and used.
- 2026-11-28, tests: `internal/seal` (also six replicas that start at once), `internal/server/credentials_test.go`, `internal/upstream`.
- 2026-11-28, in docker with the image of this repository: the key volume removed, `healthz` 200 and `readyz` 503 with `seal_key`, `docker exec ... reset-seal-key -yes`, ready 3 seconds later with no restart; with nothing done the container ended with exit code 1 after the wait. `kubectl exec` was not run.
- **Not run:** a volume in Kubernetes or a Secret shared by replicas; the output of `reset-seal-key` above is of one password.
