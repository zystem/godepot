# godepot: specification

Explanation and reference. Draft of 2026-11-16. Of this page, the database package (D1 as an interface, D4), the blob layer (S1 to S4, over blobgate), the lease (O2) and a start of the program and its metrics (O4) are built: [blob-layer.md](blob-layer.md), [status.md](status.md). Nothing else is. The plan of the work, and the traps of other products with the way round each, are in [plan.md](plan.md).

## What it is

godepot is a repository manager: it keeps the packages a team builds, keeps a copy of the packages it takes from public registries, and serves both to the usual tools (`docker`, `mvn`, `npm`, `pip`, `go`, `apt`, `dnf`, `helm`) from one address. It keeps every byte in [blobgate](https://github.com/zystem/blobgate) and nothing on a disk of its own.

blobgate's roadmap says that repository formats are "a separate service on top of this one". This is that service. blobgate stays a blob store and gains no knowledge of formats.

## When to use it, and when not

Use it when you need hosted, proxy and group repositories of several formats, on storage you do not control or of different kinds (whatever rclone talks to), with more than one replica, and without a licence that counts components, requests or seats.

Do not use it when:

- one format on one disk is enough (a plain OCI registry, or a static directory behind a web server, is less to run);
- you need a vulnerability database, licence analysis or a malware feed inside the product (godepot calls a scanner you run and applies a policy to its answer; it has no feed of its own);
- you need replication between sites that are both written to (section "Does not do").

## Where the ideas come from

Twelve products were looked at. The column "taken" is a requirement below; the column "left" is a trap in [plan.md](plan.md) or a thing out of scope. The statements about the products are from their documentation and from memory; those checked on 2026-11-16 are marked in the plan.

| Product | Taken | Left |
|---|---|---|
| Sonatype Nexus Repository | hosted / proxy / group; routing rules; cleanup policies; one program for many formats | embedded database; three tasks to free a disk; limits by tier; set up by clicks |
| JFrog Artifactory | storage by checksum (one copy of the same bytes, a copy or a move is a record); properties on artifacts; promotion; the record of a build; permission by repository and path; search by checksum | many services to run; formats and HA by licence; records of builds in a format of its own |
| Harbor | projects with quotas; robot accounts; tag immutability and retention rules; garbage collection while serving; signatures and scan results kept next to the image | OCI only; proxy projects that cannot be mixed with hosted ones; eight services |
| Pulp | a repository is a row of immutable versions; a publication is switched at once and can be rolled back; content is fetched on demand | a sync that mirrors a whole upstream first; many services; no web interface for readers |
| Zot | one small program; OCI and nothing else in its wire format; referrers; the conformance suite as a gate | one format |
| CNCF Distribution | the reference behaviour of the OCI API | deletion that needs the registry stopped; no accounts |
| Verdaccio | zero-setup start; several uplinks per package scope | one node, files on a disk; an uplink that is down fails the listing |
| devpi | an index that inherits from another (a group, with the order written) | one node |
| Athens (Go module proxy) | a fetched version is never fetched again and never changes | one format |
| aptly | snapshots of apt repositories, published under a signature, merged and rolled back | one writer, a local database |
| Cloudsmith | a cooldown for new upstream versions; trust per upstream; entitlement tokens that expire | a service only |
| GitLab / Forgejo package registries | publishing from CI with the job's own identity, no stored secret | no groups, weak proxying, weak cleanup |

## The parts

```
 docker / mvn / npm / pip / go / apt / dnf / helm          browser (read-only pages)        operator: file + CLI
            │ native protocols, one port                        │                               │
            ▼                                                   ▼                               ▼
 ┌──────────────────────────────────────────────────────────────────────────────────────────────────┐
 │ godepot (replicas, stateless)                                                                     │
 │  doors: oci, maven, npm, pypi, go, raw, apt, rpm, helm   ·  auth and policy  ·  proxy fetcher      │
 │  one replica at a time (lease): collector, publications, cleanup, upstream checks                  │
 └───────────────┬──────────────────────────────────────┬──────────────────────────────┬─────────────┘
                 │ records (components, assets, refs)    │ blobs by SHA-256             │ fetch
                 ▼                                       ▼                              ▼
          database (rqlite)                    blobgate (one tenant)           upstream registries
                                                         │ rclone
                                                         ▼
                                               nodes (S3, SFTP, WebDAV, FTP, disk)
```

## Words

- **format**: the wire protocol of one ecosystem (OCI, Maven, npm, ...). A repository has one format.
- **repository**: a named place of one format. It is **hosted** (written by you), **proxy** (a copy, made on demand, of one upstream) or **group** (an ordered list of repositories read as one).
- **upstream**: the registry a proxy repository copies from.
- **component**: one version of one package (`org.example:lib:1.2.0`, `left-pad@1.3.0`, an image manifest).
- **asset**: one file of a component (a jar, a pom, a tarball, a layer). An asset names a blob.
- **blob**: bytes, named by their SHA-256, kept once in blobgate however many assets name them.
- **reference**: a row that says "this asset (or this upload in progress) needs this blob". A blob with no reference is garbage.
- **publication**: an immutable, generated set of index files of a repository (apt `Release` and `Packages`, rpm `repodata`, a Helm `index.yaml`). A repository serves one publication; switching to the next is one transaction.
- **claim**: the statement that a name (or a prefix of names) belongs to a hosted repository and is never asked from an upstream.
- **cooldown**: the time a new upstream version waits before a proxy serves it.
- **promotion**: making a component of one hosted repository appear in another, without moving bytes.
- **robot**: an account for a program, with a scope and an end date.

## Requirements

Each has a number so that the plan and the tests can name it. "Must" is needed for 1.0; "should" may come later.

### Storage

- **S1** Every blob is kept in blobgate under the key `blobs/sha256/<2 hex>/<64 hex>/<life>`, where `<life>` is random and new each time a digest that was collected comes back, so that a late delete of the old life cannot touch the new one ([blob-layer.md](blob-layer.md)). The same bytes are kept once across all repositories and formats. (must)
- **S2** A blob is visible to a client only after its SHA-256 has been checked against what the client or the upstream named. An upload of unknown digest (an OCI chunked upload) goes to `uploads/<id>` and is made the blob by blobgate's `?link` (the store has `dedup: "tenant"`), sending nothing twice. (must)
- **S3** The record in the database is the commit, as in blobgate: a blob with no reference is a stray and is removed; a reference with no blob cannot exist, because the reference is written after the blob is checked. (must)
- **S4** Space is freed by one mechanism with no task to schedule: when the last reference goes, the blob is queued; after a grace time (default 24 hours, so that a push in progress that is about to name it is safe) it is deleted from blobgate. The collector runs while the service serves. (must)
- **S5** Small documents that change (an npm packument, a `maven-metadata.xml`, an index page) are rows in the database, not blobs, up to 256 KiB; above that they are blobs. Nodes that are slow with many small files (FTP) are then not asked for them. (must)
- **S6** godepot writes nothing to a local disk but the seal key, once (O1), and runs with a read-only root. (must)
- **S7** A download may be answered with a redirect to a presigned address of blobgate's S3 door, so that the bytes do not pass through godepot. Off by default; it needs the S3 door reachable by clients. (should)

### The database

- **D1** One database, outside the process. No embedded database in any mode, the try-it included (the try-it starts rqlite in a container, as blobgate's does). (must)
- **D2** The database is used through one narrow package, so that a second kind can be added. The first kind is rqlite. The ceiling (components, writes per second) is measured in step 2 of the plan and written in the README; if it is under the target of N1, PostgreSQL is added as the second kind before 1.0. (must)
- **D3** Everything derived (search, index pages, counts, publications) can be rebuilt from the records and is checked against them by the background replica. No task exists for an operator to "repair" or "rebuild". (must)
- **D4** The schema is changed by the program at start, forward only, one release at a time; a program refuses a schema newer than its own. (must)

### Repositories

- **R1** Hosted, proxy and group, for every format. A group may hold hosted and proxy repositories together, and other groups. (must)
- **R2** A group answers from its members in the written order, and stops at the first member that has the name. It does not merge the versions of one name from a hosted and a proxy member unless the group says `merge: true`. (must)
- **R3** **Claims are on by default.** A name published to a hosted repository is claimed for it in every group that holds it: no upstream is asked for that name. A prefix can be claimed in the settings before anything is published (`claims: ["com.example.", "@example/"]`). A request for a claimed name that the hosted repository lacks is a 404, not a fetch. (must)
- **R4** A release version is immutable: writing other bytes to an existing version is 409. Per repository, a pattern may allow overwriting (Maven `-SNAPSHOT`, OCI tags that are not in `immutable_tags`). Writing the same bytes again is 200. (must)
- **R5** Promotion: `POST /api/v1/repositories/{from}/components/{id}:promote` to a hosted repository of the same format. It writes records and references only. The component keeps its properties and the record of its build. (must)
- **R6** Properties: any component carries key and value pairs, set at upload (a header, or the native field where a format has one) or later; searchable; usable in policies. (must)
- **R7** A repository has a quota (bytes and components). The bytes of a repository are the sum of the sizes of the assets it names, shared or not; the instance also shows the bytes really kept. A write over the quota is refused with the figures. (must)

### Proxy

- **P1** On demand: nothing is copied until asked for. The bytes are sent to the client while they are written to blobgate, and are committed only when the digest matches what the upstream named (where the format names one). (must)
- **P2** Requests for the same missing asset are joined: one fetch from the upstream, many readers. (must)
- **P3** What cannot change (an asset of a release, a blob named by digest, a Go module version) is fetched once and never asked again. What can change (a tag, a packument, an index) has a time to live per repository, is asked again with `If-None-Match`, and **is served stale, with a `Warning` header and a metric, when the upstream does not answer**. (must)
- **P4** A "not found" from an upstream is remembered for a short time (default 10 minutes), per name. An error or a timeout of an upstream is never remembered as "not found", and stops requests to that upstream for a growing pause (from 5 seconds to 5 minutes), for that upstream only; what is cached is served meanwhile. (must)
- **P5** If an upstream gives other bytes for a version that is cached and cannot change, the cached bytes stay, the event is written to the audit log and a metric counts it. (must)
- **P6** Cooldown: a version that the upstream published less than `cooldown` ago (default `0`, recommended 7 days for npm and PyPI) is left out of the indexes godepot serves and is refused by name, with a message that says when it will be served. An allow list lifts it per name. The publish time is the upstream's own (`time` in a packument, `upload-time` in PyPI, `Last-Modified` otherwise). (must)
- **P7** Block and allow lists per proxy repository: names, version ranges, and digests. (must)
- **P8** An upstream has its own credentials (sealed in the database under the seal key, or taken from a variable or file the settings name), its own limits (requests per second, at once), and its own trust: `verify` says what is checked (the digest the upstream names, a signature where the format has one). Credentials are never sent after a redirect to another host. (must)
- **P9** The address of an upstream is set by the operator only. A fetch refuses private and link-local addresses after resolving the name, unless the upstream says `allow_private: true`. (must)
- **P10** A scanner is a program godepot calls with a blob (or to which it gives a presigned address) and that answers with a report; a policy maps the report to "serve", "warn" or "refuse". A scanner that is down: the policy says `fail_open` or `fail_closed` (default open for cached content, closed for new). (should)
- **P11** Downloads of blobs going on at once are limited (a setting; the default is chosen from the memory of blobgate, about 10 MiB for each), a download over the limit waits for a short time and then gets 503 with `Retry-After`, and a download that goes slower than a minimum rate for a time is ended. (must)

### Formats

A format is done when the real client passes the conformance script of the plan against hosted, proxy and group repositories.

| Tier | Format | What is served |
|---|---|---|
| 1 (must) | OCI | the OCI distribution API 1.1: images, indexes, Helm charts, any artifact; referrers; chunked and one-call uploads; mount of a blob from another repository; delete by digest; tag listing |
| 1 | raw | files under paths; `PUT`, `GET`, `HEAD`, `Range`, a listing page |
| 1 | Maven | layouts of Maven 2/3; generated `maven-metadata.xml`; `.sha1`, `.md5`, `.sha256`, `.sha512` computed, and checked when sent; snapshots with time stamps |
| 1 | npm | packuments, full and abbreviated; scopes (both `@scope/name` and `@scope%2fname`); tarballs; publish, unpublish, dist-tags; the token login of the client |
| 1 | PyPI | the simple index in HTML and JSON, with hashes, metadata files and yanked marks; upload by `twine`; names normalised |
| 1 | Go | the module proxy protocol (`list`, `.info`, `.mod`, `.zip`, `@latest`) |
| 2 (should) | apt | `InRelease`/`Release`, `Packages`, by-hash paths; upload of `.deb`; signed publications |
| 2 | rpm | `repodata`; upload of `.rpm`; signed `repomd.xml` |
| 2 | Helm (classic) | `index.yaml` generated from charts |
| 2 | NuGet, Cargo | the v3 feed; the sparse index |
| 3 (later) | Conan, RubyGems, Composer, Terraform, conda | one at a time, when asked for |

- **F1** All formats are served on one port and one host name. An OCI repository is `host/<repository>/<image>`; no port and no host name per repository. (must)
- **F2** Every address godepot writes into an index (npm tarball addresses, apt and rpm locations, the OCI `Location` and `WWW-Authenticate`) is made from the setting `external_url`. Forwarded headers are believed only from the proxies listed in `trusted_proxies` (default: none). (must)
- **F3** An index is a publication (formats apt, rpm, Helm classic): generated aside, complete, then switched in one transaction. The files of the previous publication stay readable for a grace time (default 1 hour), so that a client in the middle of an update does not meet a file that no longer matches its index. apt is served by hash. (must for tier 2)
- **F4** Publications are signed with a key godepot does not hold in the clear: the key is sealed in the database under the instance's seal key, or the signature is asked from a signer outside (a PKCS#11 token; a command). (must for tier 2)
- **F5** Every parser of a thing that comes from outside (manifests, poms, packuments, `PKG-INFO`, control files, rpm headers, archives) has limits of size, depth and count, and a fuzz target. Archives are read, never unpacked to a disk. (must)

### Access

- **A1** Accounts: local users, OIDC (single sign-on) and LDAP. All three in the one edition there is. (must)
- **A2** Tokens: every token has an owner, a scope (repositories, path patterns, actions `read` / `write` / `delete` / `promote` / `admin`) and an end date. The longest life is a setting (default 90 days). A token is shown once and stored as a digest. Robots are accounts whose names are plain words (`ci-build`, no `$`). (must)
- **A3** Identity of a CI job: a repository can trust an OIDC issuer (GitHub Actions, GitLab, Kubernetes, any CI that gives a job a signed token) with conditions on the claims (repository, branch, environment); a job exchanges its own token for a godepot token of 15 minutes. No secret is stored in the CI. (must)
- **A4** Permissions: roles bound to an account or a group of the identity provider, on a repository and optionally a path pattern. Anonymous read is a setting per repository, off by default. (must)
- **A5** The native logins of the clients work with tokens: the `docker login` token flow, npm's token login, basic authentication for Maven, pip, apt and dnf. A password or a token in an address is cut out of every log line. (must)
- **A6** Every write, delete, promotion, login, refusal and change of settings is an audit line (who, from where, what, the result), in the log and readable by API; shipped like any log. Reads are counted, and are audited when the repository says so. (must)

### Cleanup

- **C1** A policy is a list of rules per repository: keep the newest N versions of a name; keep what was read in the last D days; keep what matches a pattern or a property; remove the rest older than D days. "Keep" always wins over "remove". (must)
- **C2** A policy runs dry by default and writes a report (what would go, how many bytes that frees for real, counting shared blobs). An operator turns `enforce: true` on per repository after reading it. (must)
- **C3** What a policy or a person removes goes to a trash for a time (default 7 days, blobgate's `soft_delete_seconds` under it) and can be restored whole. (must)
- **C4** The time of the last read is kept per component to the day, in memory, and written in batches (one statement a minute at most). A read never waits for a write to the database. (must)
- **C5** An OCI manifest that an index, a referrer or a tag needs is never removed while that one stays. Removing a tag removes nothing else at once; the manifest goes when nothing names it and the policy allows it. (must)

### Operation

- **O1** Settings are a file (repositories, upstreams, policies, roles, the trust of issuers), checked by `godepot validate`, read at start and on `SIGHUP`; the same things can be read by API. Secrets come from files or the environment, never from the settings file. What godepot shows to others (upstreams, the directory) may also be given with the API; it is then sealed in the database under the seal key, a file that godepot makes when there is none and the database has no key yet; a lost key is answered by a command that forgets the sealed secrets so that they can be given again. The web pages change nothing. (must)
- **O2** Any number of replicas; none is special for clients. Background work is done by the replica that holds a lease in the database. (must)
- **O3** The service serves TLS itself and works with no reverse proxy in front; the page for a reverse proxy says the three settings that matter (body size without limit, no buffering of bodies, timeouts). (must)
- **O4** Prometheus metrics for every door, every upstream (latency, errors, stale answers, cooldown refusals), the collector, the cache; `/healthz`, `/readyz`. (must)
- **O5** Events: a webhook per repository on publish, delete, promote, a policy refusal; delivered at least once, signed. (should)
- **O6** Backup is the backup of the database (blobgate's `backup` for rqlite) plus blobgate's own nodes; `godepot verify` then checks every reference against blobgate and names what is missing. A restore of an older database over newer blobs is safe: blobs that the old records do not name are strays and wait the grace time. (must)
- **O7** Out: `godepot export <repository> <dir>` writes a repository in the plain layout of its format (a Maven tree, an OCI layout, a directory of tarballs with indexes) that a static web server could serve. In: `godepot import` reads the same layouts, and reads a Nexus or an Artifactory over their REST APIs (repositories, assets, checksums, properties). (must: export, plain import; should: the two importers)
- **O8** Web pages, read-only: browse repositories, search by name, version, digest and property, see a component (assets, sizes, digests, who published, the build, scan results, who depends on the blob), copy the setup lines for a client. Files uploaded by users (raw HTML, READMEs) are served with `Content-Security-Policy: sandbox` and `nosniff`. (should)

### Numbers

- **N1** Targets for 1.0, to be measured, on one replica with 2 CPUs and 512 MiB: 1 million components, 10 million assets; 500 cached reads a second of small assets; a 10 GiB layer pushed and pulled without the memory rising above the limit; a cold start under 5 seconds. Each figure in the README is one that was run, with the date. (must)
- **N2** No limit that is not a physical one or a quota the operator set. (must)

## The data model

| Table | Holds |
|---|---|
| `repositories` | name, format, kind, settings version |
| `components` | repository, namespace, name, version, format fields, created by, created at, last read (day), deleted at |
| `assets` | component, path, blob digest, size, content type, the digests a format needs (sha1, md5, sha512) |
| `blobs` | digest, size, key, state (incoming, live), unreferenced since |
| `refs` | owner (an asset, an upload, a publication), blob digest |
| `uploads`, `graves` | bytes being taken in; keys of the store that are to be deleted |
| `tags` | repository, name, the manifest it names (OCI tags, npm dist-tags) |
| `links` | from manifest, to manifest (an index and its children; a subject and its referrers) |
| `documents` | repository, path, body up to 256 KiB, validators (packuments, metadata, cached upstream indexes) |
| `publications` | repository, number, state, the list of its files |
| `claims` | group, name or prefix, hosted repository |
| `upstream_state` | upstream, validators, negative cache, pause until |
| `properties`, `builds` | key and value pairs; the provenance of a build (an in-toto statement), by digest |
| `accounts`, `tokens`, `bindings`, `audit` | access |
| `lease`, `queue` | who does the background work; what is to do (collect, publish, scan, notify) |

A write of a component is one transaction: the component, its assets, the references, the count of each blob, the queue entries. A delete is one transaction that marks the component and queues the rest.

## What blobgate must give

Checked against blobgate 0.6.0 on 2026-11-16 by reading its documents; none of it was run for this page.

| Need | In blobgate today | Gap |
|---|---|---|
| write a blob of unknown size and digest in parts | uploads in parts of at most 8 MiB, all but the last of one size | none; godepot cuts the stream into 8 MiB parts itself |
| make `blobs/sha256/..` from an upload without sending it again | `?link` with `{sha256, size}`, store with `dedup: "tenant"` | none: run for a 10 GiB object on one local node ([step0-probes.md](step0-probes.md)) |
| read a range | yes | a read holds a block (8 MiB) in memory, **kept for as long as the client is slow**: about 10 MiB of blobgate for each download going on (measured, [step0-probes.md](step0-probes.md)); godepot limits downloads (P11 below) |
| trash and restore | `soft_delete_seconds`, `?restore`, `?purge` | none |
| quota of the whole instance | per tenant and store | none; quotas per repository are godepot's own |
| presigned reads (S7) | the S3 door, up to 7 days | optional |
| list blobs, to find strays | listing with a cursor | none |
| many small objects | a row each; slow on FTP nodes | avoided by S5 |

godepot is one tenant of blobgate. A second instance of godepot is a second tenant; they share nothing.

## Does not do

- A vulnerability database, a licence database or a malware feed of its own (P10 calls yours).
- Replication between two sites that are both written to. One site writes; another may be a proxy of it (which is a full, on-demand, read-only copy).
- Settings made in the browser.
- Plugins loaded at run time. A format is a package of the program.
- A Docker registry API v1, Maven 1, or other protocols that their own clients dropped.
- A build system, a CI, or a place for source code.

## Open decisions (the owner's)

1. **The second database** (D2): add PostgreSQL only if rqlite does not reach N1, or from the start. The recommendation is the first: one dependency fewer, the same one blobgate already needs.
2. **Tier 1 order.** The plan takes OCI first (the hardest for storage), then raw, Maven, npm, PyPI, Go. Another order costs nothing if said before step 3.
3. **The web pages** (O8): in 1.0, or after. The plan puts them last.
