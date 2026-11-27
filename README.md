# godepot

**godepot is a repository manager (packages and images for `docker`, `mvn`, `npm`, `pip`, `go`) that keeps every byte in [blobgate](https://github.com/zystem/blobgate), once, and frees the disk by itself.**

> **Status, 2026-11-28: early, before 1.0** (settings and tables may still change). **Six formats are served**, each as hosted, proxy and group repositories: `raw` (files by path, with `curl`), `oci` (images, Helm charts and any artifact), `go`, `pypi`, `npm` and `maven`. People and programs get in with tokens, with the password of a local account or of LDAP or Active Directory, and CI jobs with their own signed token (OIDC). Also: cleanup with a trash, cooldown and block lists for proxies, an audit log, metrics. **Not built:** apt, rpm and Helm repositories, promotion and builds, single sign-on of people in a browser, web pages, TLS on its own port, a published image and signed releases. What is built, what was run and what was not: [docs/status.md](docs/status.md); what comes next: [docs/spec.md](docs/spec.md) and [docs/plan.md](docs/plan.md).

## When to use it, and when not

Use it (it is early: try it before you depend on it) when you need hosted, proxy and group repositories of several formats on storage you do not control or of different kinds (whatever rclone talks to), with no licence that counts components, requests or seats.

Do not use it when one format on one disk is enough (a plain registry, or a static directory behind a web server, is less to run), when you need a vulnerability or licence database inside the product (godepot will call a scanner you run), when you need a format or a feature of the "Not built" list above, or when you cannot put a reverse proxy with TLS in front of it.

## The parts

```
 curl    docker / helm    go    pip / twine    npm    mvn
            │
            ▼
 ┌─────────────────────────────────────────────┐
 │ godepot (any number of replicas, stateless)  │
 │  accounts, tokens, audit; the six formats    │
 │  blob layer: bytes by digest, references     │
 │  collector (one replica at a time: a lease)  │
 └──────────┬──────────────────────┬────────────┘
            │ records              │ bytes
            ▼                      ▼
         rqlite                 blobgate ──rclone──▶ nodes (S3, SFTP, WebDAV, FTP, a disk)
```

## Try it (a few minutes, Docker only)

This starts a real blobgate and godepot, makes an account and a token, and writes, reads and lists a file with `curl`. It needs nothing but Docker.

```bash
docker build -t godepot .
docker run -d --name bg-rq -p 4001:4001 rqlite/rqlite -fk -disco-mode=
docker run -d --name gd-rq -p 4011:4001 rqlite/rqlite -fk -disco-mode=
docker run -d --name blobgate --network host --read-only --tmpfs /data:rw,mode=1777,size=2g \
  -e BLOBGATE_RQLITE_URL=http://127.0.0.1:4001 -e BLOBGATE_PORT=18090 -e BLOBGATE_SHIM_PORT=0 \
  -e BLOBGATE_TENANTS=godepot=a-godepot-secret-16-chars -e BLOBGATE_ADMIN_TOKEN=an-admin-token-16-chars \
  -e BLOBGATE_SEAL_KEY=a-seal-key-16-chars-long ghcr.io/zystem/blobgate:0.6.1
sleep 5
```

Make the store godepot will use. It must be set to `dedup: "tenant"` (godepot makes the key of a blob from the bytes of an upload by a link, and the store has to be able to):

```bash
curl -s -X PUT localhost:18090/v1/admin/stores/main -H "Authorization: Bearer an-admin-token-16-chars" \
  -d '{"kind":"mirror","default":true,"dedup":"tenant","nodes":[{"remote":":local:/data/n1"},{"remote":":local:/data/n2"}]}'
```

```
{"checked":true,"name":"main"}
```

Write the settings file: one repository, and a robot that may read and write in it ([docs/settings-file.md](docs/settings-file.md)):

```bash
cat > godepot.json <<'EOF'
{
  "repositories": [{"name": "files", "format": "raw", "kind": "hosted"}],
  "grants": [{"account": "ci-build", "repositories": ["files"], "actions": ["read", "write"]}]
}
EOF
G="docker run --rm --network host --read-only -v $PWD/godepot.json:/godepot.json:ro -e GODEPOT_CONFIG_FILE=/godepot.json -e GODEPOT_RQLITE_URL=http://127.0.0.1:4011 -e GODEPOT_BLOBGATE_URL=http://127.0.0.1:18090 -e GODEPOT_BLOBGATE_TENANT=godepot -e GODEPOT_BLOBGATE_SECRET=a-godepot-secret-16-chars -e GODEPOT_BLOBGATE_STORE=main godepot"
$G validate
```

```
settings: ok
settings file: ok (1 repositories, 1 grants)
database: ok
store: ok
```

Make the tables, the account and a token for it (here the account gets in with a token, which has a scope and an end date; a user may also have a password or come from a directory, [docs/access.md](docs/access.md)):

```bash
$G migrate
$G new-account ci-build -robot 2>/dev/null
TOKEN=$($G new-token ci-build -life 24h -repositories files -actions read,write 2>token.err)
cat token.err
```

```
the schema of the database is at version 8
account ci-build (robot) made
token 31b258b6a231 for ci-build, valid until 2026-11-28T23:22:33Z. This is the only time it is shown; it is not kept.
```

(`new-account` and `new-token` also write a line of the audit log, as JSON, to the standard error; it is cut here. The token is on the standard output, so it is in `$TOKEN` and not on the screen. The first line is `migrate`'s.)

Run it, and write, read and list a file:

```bash
docker run -d --name godepot --network host --read-only -v $PWD/godepot.json:/godepot.json:ro -e GODEPOT_CONFIG_FILE=/godepot.json -e GODEPOT_RQLITE_URL=http://127.0.0.1:4011 \
  -e GODEPOT_BLOBGATE_URL=http://127.0.0.1:18090 -e GODEPOT_BLOBGATE_TENANT=godepot -e GODEPOT_BLOBGATE_SECRET=a-godepot-secret-16-chars -e GODEPOT_BLOBGATE_STORE=main \
  -e GODEPOT_METRICS_PORT=18101 godepot serve
sleep 5
echo hello > hello.txt
curl -s -u x:$TOKEN -X PUT --data-binary @hello.txt -H "Content-Type: text/plain" localhost:18100/repository/files/docs/hello.txt
curl -s -u x:$TOKEN localhost:18100/repository/files/docs/hello.txt
curl -s -u x:$TOKEN -H "Range: bytes=0-3" localhost:18100/repository/files/docs/hello.txt; echo
curl -s -u x:$TOKEN localhost:18100/repository/files/docs/
```

```
{"path":"docs/hello.txt","sha256":"5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03","size":6}
hello
hell
{"items":[{"path":"docs/hello.txt","size":6,"sha256":"5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03","content_type":"text/plain","created_at":1795821759}]}
```

A file is immutable (other bytes at the same path are refused, the same bytes are fine), and a request with no token is turned away:

```bash
curl -s -o /dev/null -w "%{http_code}\n" -u x:$TOKEN -X PUT --data-binary @hello.txt localhost:18100/repository/files/docs/hello.txt
echo bye | curl -s -w "\n%{http_code}\n" -u x:$TOKEN -X PUT --data-binary @- localhost:18100/repository/files/docs/hello.txt
curl -si localhost:18100/repository/files/docs/hello.txt | head -3
curl -s localhost:18101/metrics | grep -E "^godepot_(writes|reads|refused)_total"
```

```
200
{"error":"the path is there with other bytes; a file is not overwritten in this repository","info":{"sha256":"5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"}}

409
HTTP/1.1 401 Unauthorized
Content-Type: application/json
Www-Authenticate: Basic realm="godepot", charset="UTF-8"
godepot_reads_total{repo="files"} 2
godepot_writes_total{repo="files"} 1
godepot_refused_total{reason="conflict"} 1
godepot_refused_total{reason="unauthenticated"} 1
```

Last, compare the store with the database (`$G reconcile`):

```
checked 2 live blobs
would remove (dry: nothing was changed, use -fix) 0 keys that nothing names
```

Two blobs, not one: the bytes of the refused write (`bye`) were taken in and are kept until the grace time (24 hours) has passed, in case a client was about to name them; then the collector removes them.

Run on 2026-11-28 with the image of this repository and blobgate 0.6.1, as shown. The outputs are the ones that run printed, except the lines said above to be cut, and the token and its id, which are different every time; the `created_at` and the end date are those of that run. Not run by a test: a test that runs this page is still to do. Remove it all with `docker rm -f godepot blobgate bg-rq gd-rq`.

**Next:** [docs/proxy-howto.md](docs/proxy-howto.md) (a proxy of Docker Hub and a group), [docs/oci-clients.md](docs/oci-clients.md) (the same account and token push images and charts), [docs/raw.md](docs/raw.md) (every request of the format), [docs/access.md](docs/access.md) (accounts, scopes: why it is so), [docs/api.md](docs/api.md) (the API, the audit log).

## Ways to install

| Way | For | Notes |
|---|---|---|
| `docker build .` | tests, the try-it | a static program in an image with no shell, user 65532, a read-only root; nothing is written to a disk |
| `go build ./cmd/godepot` (Go 1.26) | a workstation, changes | |
| an image on a registry, a chart, signed releases | production | **not built yet**: a release is a tag and its notes; build it yourself from the tag |

## Words

* **blob**: bytes named by their SHA-256, kept once in blobgate however many owners need them.
* **repository**: a named place of one format, set in the settings file. It is `hosted` (written by you), a `proxy` (a copy of an upstream, made on demand) or a `group` (several read as one), and has one format.
* **account**, **token**: who, and what they carry to get in. An account gets in with tokens (a user also with a password, its own or a directory's); a token has an owner, a scope and an end date, and is shown once.
* **audit log**: a line, in the log and in the database, for every write, delete, refusal and change of accounts.
* **reference**: a record that an owner (an asset, an upload in progress) needs a blob. A blob nothing names is garbage after a grace time.
* **upload**: bytes being taken in; they are held for their owner-to-be until it commits what they are for.
* **grave**: a record that a key of the store is to be deleted, written in the same transaction that makes it unneeded.
* **lease**: the right to do the background work, held by one replica at a time.
* **store**, **tenant**, **node**: as in blobgate.

## The other documents

| You want to | Read |
|---|---|
| know what it will be | [docs/spec.md](docs/spec.md), [docs/plan.md](docs/plan.md) (with the traps of other products, and the way round each) |
| know how bytes are kept, and what a crash leaves | [docs/blob-layer.md](docs/blob-layer.md) |
| set it up | [docs/configuration.md](docs/configuration.md) (every setting, command and exit code), [docs/settings-file.md](docs/settings-file.md) (repositories and grants) |
| connect | [docs/proxy-howto.md](docs/proxy-howto.md) (Docker Hub and your own images behind one address), [docs/oci-clients.md](docs/oci-clients.md) (`helm`, `oras`, `crane`, `skopeo`, `cosign` against the format `oci`), [docs/oci.md](docs/oci.md) (every request of it), [docs/raw.md](docs/raw.md) (the format `raw`), [docs/gomod.md](docs/gomod.md), [docs/pypi.md](docs/pypi.md), [docs/npm.md](docs/npm.md), [docs/maven.md](docs/maven.md) (the formats `go`, `pypi`, `npm`, `maven`), [docs/api.md](docs/api.md) (tokens, the API, the audit log), [docs/ci-jobs.md](docs/ci-jobs.md) (publish from CI with no stored secret), [docs/ldap.md](docs/ldap.md) (log in with LDAP or Active Directory), [docs/access.md](docs/access.md) (how it works and why) |
| watch it | [docs/metrics.md](docs/metrics.md) |
| change it | [docs/testing.md](docs/testing.md), [docs/status.md](docs/status.md), [docs/README.md](docs/README.md) (the list of pages and their kinds) |

## Not there yet

The things a project of this kind needs and that this one does not have: a contributing guide, a security policy, signed releases, the OpenSSF badge, a changelog, a troubleshooting page, a production checklist. The plan ([docs/plan.md](docs/plan.md)) meant them from the first release; the releases so far are pre-releases without them.
