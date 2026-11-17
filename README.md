# godepot

**godepot is a repository manager (packages and images for `docker`, `mvn`, `npm`, `pip`, `go`, `apt`) that keeps every byte in [blobgate](https://github.com/zystem/blobgate), once, and frees the disk by itself.**

> **Status, 2026-11-17: early. No format is served yet.** What is built is the part that is hardest to get right and that other products get wrong: the layer that keeps bytes by their SHA-256, knows who needs them, and collects what nobody needs while the service serves ([docs/blob-layer.md](docs/blob-layer.md)). A client (`docker`, `npm`, ...) cannot talk to this release. The specification and the plan are in [docs/spec.md](docs/spec.md) and [docs/plan.md](docs/plan.md); [docs/status.md](docs/status.md) says what is built and what was run.

## When to use it, and when not

Use it (when it is done) when you need hosted, proxy and group repositories of several formats on storage you do not control or of different kinds (whatever rclone talks to), with more than one replica and no licence that counts components, requests or seats.

Do not use it when one format on one disk is enough (a plain registry, or a static directory behind a web server, is less to run), when you need a vulnerability or licence database inside the product (godepot will call a scanner you run), or **today for anything but reading the code**.

## The parts

```
 docker / mvn / npm / pip / go / apt ...        (formats: not built yet)
            │
            ▼
 ┌─────────────────────────────────────────────┐
 │ godepot (any number of replicas, stateless)  │
 │  blob layer: bytes by digest, references     │
 │  collector (one replica at a time: a lease)  │
 └──────────┬──────────────────────┬────────────┘
            │ records              │ bytes
            ▼                      ▼
         rqlite                 blobgate ──rclone──▶ nodes (S3, SFTP, WebDAV, FTP, a disk)
```

## Try it (a few minutes, Docker only)

This shows that the program runs against a real blobgate and checks it can use it. It does not store a package: there is no way yet to push one.

```bash
docker build -t godepot .
docker run -d --name bg-rq -p 4001:4001 rqlite/rqlite -fk -disco-mode=
docker run -d --name gd-rq -p 4011:4001 rqlite/rqlite -fk -disco-mode=
docker run -d --name blobgate --network host --read-only --tmpfs /data:rw,mode=1777,size=2g \
  -e BLOBGATE_RQLITE_URL=http://127.0.0.1:4001 -e BLOBGATE_PORT=18090 -e BLOBGATE_SHIM_PORT=0 \
  -e BLOBGATE_TENANTS=godepot=a-godepot-secret-16-chars -e BLOBGATE_ADMIN_TOKEN=an-admin-token-16-chars \
  -e BLOBGATE_SEAL_KEY=a-seal-key-16-chars-long ghcr.io/zystem/blobgate:0.6.1
```

Make the store godepot will use. It must be set to `dedup: "tenant"` (godepot makes the key of a blob from the bytes of an upload by a link, and the store has to be able to):

```bash
curl -s -X PUT localhost:18090/v1/admin/stores/main -H "Authorization: Bearer an-admin-token-16-chars" \
  -d '{"kind":"mirror","default":true,"dedup":"tenant","nodes":[{"remote":":local:/data/n1"},{"remote":":local:/data/n2"}]}'
```

```
{"checked":true,"name":"main"}
```

Check the settings, the database and the store (`G` is the long `docker run` of godepot; the settings are in [docs/configuration.md](docs/configuration.md)):

```bash
G="docker run --rm --network host --read-only -e GODEPOT_RQLITE_URL=http://127.0.0.1:4011 -e GODEPOT_BLOBGATE_URL=http://127.0.0.1:18090 -e GODEPOT_BLOBGATE_TENANT=godepot -e GODEPOT_BLOBGATE_SECRET=a-godepot-secret-16-chars -e GODEPOT_BLOBGATE_STORE=main godepot"
$G validate
```

```
settings: ok
database: ok
store: ok
```

Make the tables, and look at what is stored (nothing):

```bash
$G migrate
$G reconcile
```

```
the schema of the database is at version 2
checked 0 live blobs
would remove (dry: nothing was changed, use -fix) 0 keys that nothing names
```

Run it, give it a few seconds to start (it waits for the database and tries the store first), and ask it whether it is well and what it has done:

```bash
docker run -d --name godepot --network host --read-only -e GODEPOT_RQLITE_URL=http://127.0.0.1:4011 -e GODEPOT_BLOBGATE_URL=http://127.0.0.1:18090 \
  -e GODEPOT_BLOBGATE_TENANT=godepot -e GODEPOT_BLOBGATE_SECRET=a-godepot-secret-16-chars -e GODEPOT_BLOBGATE_STORE=main -e GODEPOT_METRICS_PORT=18101 godepot serve
curl -s localhost:18100/readyz
curl -s localhost:18101/metrics | grep "^godepot_leader"
```

```
{"checks":{"database":"ok"},"status":"ready"}
godepot_leader 1
```

Run on 2026-11-17 with the image of this repository and blobgate 0.6.1, as shown; the outputs of `validate`, `migrate`, `reconcile` and `readyz` are the ones that run printed, and the metrics were the whole page, cut here to one line. Remove it all with `docker rm -f godepot blobgate bg-rq gd-rq`.

## Ways to install

| Way | For | Notes |
|---|---|---|
| `docker build .` | tests, the try-it | a static program in an image with no shell, user 65532, a read-only root; nothing is written to a disk |
| `go build ./cmd/godepot` (Go 1.26) | a workstation, changes | |
| an image on a registry, a chart, signed releases | production | **not built yet**; the release process is a step of the plan |

## Words

* **blob**: bytes named by their SHA-256, kept once in blobgate however many owners need them.
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
| set it up | [docs/configuration.md](docs/configuration.md) (every setting, command and exit code) |
| watch it | [docs/metrics.md](docs/metrics.md) |
| change it | [docs/testing.md](docs/testing.md), [docs/status.md](docs/status.md), [docs/README.md](docs/README.md) (the list of pages and their kinds) |

## Not there yet

The things a project of this kind needs and that this one does not have: a contributing guide, a security policy, signed releases, the OpenSSF badge, a changelog. They are steps of the plan ([docs/plan.md](docs/plan.md)), to be done before the first release, not after.
