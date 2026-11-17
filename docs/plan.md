# godepot: plan

Explanation. Draft of 2026-11-16. Step 2 (the blob layer) and most of step 1 (the skeleton) are built and run against a real rqlite and a real blobgate; [status.md](status.md) says what, and what is not. Steps 0, 3 and the rest are not done (of step 0, the 2 GiB and the real-blobgate runs are). The requirements (S1, R3, P6, ...) are those of [spec.md](spec.md).

The plan has two halves: the steps, each with what proves it is done, and the traps of other products, each with the way round it and the step that proves the way round works.

## Rules of the work

- A step ends with a release that runs. No step leaves a half-built thing behind a flag.
- A step is done when its proof runs in CI, not when the code is written. The proofs use the real clients (`docker`, `mvn`, `npm`, `pip`, `go`, `apt`, `dnf`, `helm`) in containers, pinned by version.
- Every trap of the table below has a test named after it (`TestTrapN2DiskIsFreedWithNoTask`). A trap without a test is not avoided, only hoped.
- The project starts as blobgate did: the documentation rules, a try-it in the README that a test runs, signed releases, coverage of 80 %, a fuzz target per parser, the OpenSSF badge (passing, silver, the three baselines) from the first release.
- Go, one static program, the same shape as blobgate (`cmd/godepot`, `internal/...`), blobgate's Go client for the bytes.

## The steps

| # | Step | Gives | Proof that it is done |
|---|---|---|---|
| 0 | **Probe blobgate** (done 2026-11-17: [step0-probes.md](step0-probes.md)) | the gaps of blobgate known before anything stands on them | a 10 GiB object written in 8 MiB parts and made a blob by `?link`; 200 slow readers of ranges with the memory measured; 1 million small objects listed; `kill -9` between upload and link. Findings go to blobgate's issues; this plan is corrected |
| 1 | **Skeleton** | the program, the settings file and `validate`, the database package and the schema, the lease, metrics, the audit log, the try-it | the README try-it run by a test; a setting without a line in its page fails the build |
| 2 | **The blob layer** | S1 to S6: blobs by digest, references, the collector with its grace time, `verify`, the trash | a crash at every step of a write and of a delete leaves nothing unknown (blobgate's `TestCrashAtEveryStep`, one level up); a push and the collector racing 10 000 times lose no blob; **the ceiling of rqlite measured** (D2) and the decision on PostgreSQL taken |
| 3 | **raw, hosted** + accounts and tokens | the smallest format end to end; A1 (local), A2, A4, A6; R4, R6, R7 | `curl` puts, reads a range, lists; a token out of scope is refused and audited |
| 4 | **OCI, hosted** | F1, C5; chunked uploads, mounts, referrers, delete | the OCI conformance suite, all four groups; `docker`, `podman`, `helm`, `oras`, `cosign` push and pull; a 10 GiB layer within the memory limit |
| 5 | **Proxy and group** (for raw and OCI) | P1 to P5, P8, P9; R1 to R3 | an upstream that is slow, that dies in the middle of a body, that answers 429, that answers 500, that changes bytes: each is a test with a false upstream; 100 clients asking one cold layer make one fetch |
| 6 | **Maven, npm, PyPI, Go** (hosted, proxy, group; one release each) | tier 1 complete; F2, F5 | per format, the script: publish with the real client, install from hosted, from a proxy of the public registry, from a group with a claim; the same behind a reverse proxy with another host name |
| 7 | **Policies** | P6, P7 (cooldown, lists); C1 to C4 (cleanup, dry by default); R5 (promotion); builds | a version one hour old is hidden and named in the refusal; a policy report equals what enforcing then removes; the bytes freed equal the report |
| 8 | **CI identity and single sign-on** | A1 (OIDC, LDAP), A3, A5 | a job of GitHub Actions publishes with no stored secret (whether Woodpecker gives a job such a token was not checked); the LDAP tests run against the stand `dc1.test.local` |
| 9 | **apt, rpm, Helm classic** | F3, F4: publications, signatures | 50 uploads during `apt update` loops of 20 clients: no "Hash Sum mismatch"; a rollback to the previous publication; the signing key never in the clear in a dump of the database |
| 10 | **Out and in** | O6, O7: export, import, the importers for Nexus and Artifactory | a repository exported and served by a plain web server installs with the real client; a Nexus and an Artifactory in containers, filled by script, imported with every checksum equal |
| 11 | **Scanner hook, events, web pages** | P10, O5, O8 | a scanner that is down, with both `fail_open` and `fail_closed`; a page with a hostile README runs no script |
| 12 | **1.0** | N1 measured; a soak of 7 days under load with a node of blobgate taken away and returned; the production checklist, troubleshooting by message, FAQ, upgrade page | the figures in the README, with dates; an upgrade from every 0.x release in CI |

Tier 2 formats NuGet and Cargo, and tier 3, come after 1.0, one per release, each by the script of step 6.

The order inside the plan follows the risk: the steps that can show the design wrong (0, 2, 4, 5) come first, and the formats that only add work come after.

## The traps, and the way round each

"Seen" says how the statement is known: **checked** means read in the vendor's own pages or a search of them on 2026-11-16; **memory** means known from use and reading before, not checked again for this page (check it before quoting it elsewhere). Nothing was reproduced on a running Nexus, Artifactory or Harbor.

### Storage and the disk

| # | Trap | Where, seen | The way round | Proved in |
|---|---|---|---|---|
| T1 | Deleting frees nothing. A cleanup policy only marks; for Docker a second task removes unused manifests; a third ("compact blob store") frees the disk; blobs with no record are seen by none of them | Nexus; checked (the documented order is: delete incomplete uploads, cleanup, delete unused manifests, compact; forum reports of blobs that no task finds) | S3, S4: one count of references kept in the transaction that writes or removes; one collector; strays found by listing blobgate against the records | step 2 |
| T2 | Garbage collection needs the registry stopped or read-only | Distribution; Harbor before 2.1; checked for Harbor (read-only in 2.0, while serving since 2.1) | S4: the collector deletes only blobs unreferenced for longer than the grace time, and takes the reference count in a transaction, so a push needs no lock | step 2 (the race test) |
| T3 | The collector removes a layer that a push in progress is about to name | every mark-and-sweep registry; memory | a reference is added only in one transaction with the check that the blob is live, and a blob is removed only in one transaction with the check that nothing names it; the side that loses is refused whole and the client sends the bytes; a blob unnamed for less than the grace time is never removed | step 2 (built: `TestRaceOfPushAndCollect`, `TestRaceOfHoldAndCollect`, `TestALateDeleteDoesNotRemoveABlobThatCameBack`) |
| T4 | An index image loses a child, or a signature loses its subject, in cleanup; untagged manifests pile up, or deleting them breaks signed images | Harbor (deleting untagged is a switch; an issue reports signatures blocking it); checked | C5: `links` rows; a manifest goes only when no tag, no index and no subject needs it, and its referrers go with it | step 4 |
| T5 | Two files per blob on a disk (bytes and properties): inodes run out, backups crawl | Nexus file blob store; memory | S5, S6: nothing on a local disk; properties are rows | step 2 |
| T6 | The trash or the cache of a proxy grows unseen until the disk is full | Artifactory (trash can, remote caches); memory | the trash has an end date (C3); a proxy repository takes a cleanup policy and a quota like any other; a metric and the page show bytes per repository, in trash, awaiting collection | step 7 |
| T7 | The database and the blobs are backed up at different moments and do not match after a restore; a "reconcile" task is run by hand | Nexus; memory | O6: an older database over newer blobs is the safe direction by design; `verify` names what is missing; the page on backup says the order | step 2, step 12 |

### The database and derived state

| # | Trap | Where, seen | The way round | Proved in |
|---|---|---|---|---|
| T8 | An embedded database that breaks on a hard stop, and forced moves from one to the next across releases | Nexus (OrientDB, then H2, then PostgreSQL); Artifactory starts on Derby; memory | D1: one outside database from the first try-it. D4: one way of upgrading | step 1, step 12 |
| T9 | Search, browse trees and repository metadata drift from the truth; "rebuild" and "repair" tasks exist for each | Nexus; memory | D3: search is a query on the records; indexes are generated from the records in the same transaction or as a publication; the background replica compares and corrects, and counts corrections in a metric that should stay at 0 | step 6, step 9 |
| T10 | Every download writes "last downloaded" to the database; reads slow down and the database grows | Nexus, Artifactory; memory | C4: to the day, in memory, in batches | step 7 (a read test with the database stopped for a minute) |
| T11 | A large delete or cleanup holds the database and stalls pushes | all; memory | deletes mark in one short transaction; the queue works in small batches with a pause | step 7 |

### Proxying

| # | Trap | Where, seen | The way round | Proved in |
|---|---|---|---|---|
| T12 | **Dependency confusion**: a group asks the public registry for an internal name, and serves the public package when its version is higher. The guards (routing rules, priority resolution, exclude patterns) are off until someone sets them | Nexus, Artifactory; memory | R2, R3: first member wins and does not merge; claims on by default; a claimed name is never sent upstream (which also keeps internal names out of the logs of public registries) | step 5, step 6 |
| T13 | One error of an upstream marks the whole remote "offline" for a time, or a miss is cached and a version published a minute ago is "not found" for hours | Artifactory (assumed offline period, missed retrieval cache); memory | P4: errors are never cached as "not found"; the pause is per upstream and short; the negative cache is short and per name, and is cleared by the API | step 5 |
| T14 | The upstream is down and builds fail though everything needed is cached, because the index cannot be refreshed | Verdaccio (uplinks), Nexus (unless set); memory | P3: stale indexes are served, marked | step 5 |
| T15 | A cold cache and 50 CI jobs: 50 fetches of the same layer, and the upstream's rate limit is hit (Docker Hub answers 429) | all; memory | P2 (joined fetches); P8 (credentials and limits per upstream); manifests by digest are never asked twice; tag checks use `HEAD` and the time to live | step 5 |
| T16 | The proxy downloads the whole file before the first byte goes to the client; the client times out on large layers | Nexus, older Artifactory; memory | P1: stream while storing | step 5 (a 5 GiB layer through a slow upstream) |
| T17 | A malicious version is installed minutes after it is published upstream | every plain proxy; checked (cooldown is a setting of pnpm and npm and a policy of Cloudsmith; in Nexus and Artifactory the like is sold apart: Firewall, Curation) | P6, P7: cooldown and lists in the one edition | step 7 |
| T18 | An upstream replaces the bytes of a released version and the cache follows | all that refresh assets; memory | P3, P5: fetched once, kept, the change is an alarm | step 5 |
| T19 | The proxy is made to fetch from inside the network, or sends the upstream's credentials to another host after a redirect | a class of bugs (server-side request forgery, leaked `Authorization`); memory | P8, P9 | step 5 |
| T20 | A proxy cannot be pushed to and cannot be mixed with hosted repositories, so each client needs two addresses | Harbor (proxy projects), Distribution (pull-through); memory | R1: a group of hosted and proxy members, one address; a write to a group goes to the member the group names as `write_to` | step 5 |

### Formats

| # | Trap | Where, seen | The way round | Proved in |
|---|---|---|---|---|
| T21 | A port or a host name per Docker repository, and a reverse proxy that must be set up to split them | Nexus (connectors), Artifactory (three access methods); memory | F1: `host/<repository>/<image>`, one port | step 4 |
| T22 | Wrong addresses in indexes behind a reverse proxy: npm tarballs point to `http://localhost:8081`, the Docker token realm is wrong | all; memory | F2: `external_url` is required when not on localhost; `validate` warns; forwarded headers only from trusted proxies | step 6 (the reverse proxy leg) |
| T23 | `apt update` or `dnf` fails with a hash mismatch while packages are being uploaded; metadata is rebuilt by a task that runs late or twice at once | Nexus (yum and apt metadata tasks), Artifactory (asynchronous calculation); memory | F3: publications, switched in one transaction, the previous kept for a grace time, apt by hash | step 9 |
| T24 | The signing key of a repository lies in a file or in the database in the clear | several; memory | F4: sealed, or a signer outside | step 9 |
| T25 | npm: scoped names break at the reverse proxy (`%2f` decoded); huge packuments are sent in full to clients that ask for the short form | all; memory | both spellings accepted; the abbreviated form served and cached apart; packuments over 256 KiB are blobs (S5) | step 6 |
| T26 | PyPI: names not normalised (`Foo_Bar` and `foo-bar` become two packages); no hashes or metadata files in the index, so `pip` downloads wheels to resolve | older Nexus, simple proxies; memory | normalised names as the key; hashes and metadata files always | step 6 |
| T27 | Maven: a release is overwritten by a second deploy; `maven-metadata.xml` of a group lists versions of one member only; snapshot files pile up | all (redeploy is a setting); memory | R4 (immutable by default); metadata generated from the records of the member that answers (R2); a default rule "keep 5 time stamps per snapshot" proposed by `validate` | step 6, step 7 |
| T28 | A parser is fed a bomb or a path with `..`; the server reads a file of its own | Nexus (a path traversal in 2024, expression injection before); memory, the identifiers were not looked up | F5 (limits, fuzzing, archives never unpacked); a key is checked part by part, and no key is ever a path on a disk because there is no disk (S6) | every format step |
| T29 | Content that users upload runs scripts in the origin of the administration pages | Nexus (raw repositories), any registry that renders READMEs; memory | O8: sandbox headers; the pages change nothing, so a script has nothing to click | step 11 |

### Access and operation

| # | Trap | Where, seen | The way round | Proved in |
|---|---|---|---|---|
| T30 | Limits and features by edition: components and requests per day are capped and writes are then refused; single sign-on, staging and HA are paid; most formats are paid | Nexus Community Edition (checked: the tiers page read on 2026-11-16 gives 40 000 components and 100 000 requests a day, older pages 100 000 and 200 000; SAML and staging are listed as Pro, read from a flattened table); Artifactory OSS serves Maven-family formats only (memory) | N2, A1: one edition; the limits are quotas the operator sets | by licence, not by test |
| T31 | Tokens that never end, shared by a team, pasted in CI settings; a robot's name with `$` that shells eat | Artifactory (API keys, since retired), Harbor (`robot$name`), all; memory | A2 (an end date always, a longest life), A3 (no stored secret in CI), plain names | step 3, step 8 |
| T32 | Passwords in addresses (`https://user:pass@host/...` for pip, apt) land in logs | all; memory | A5: cut from every log line; a test greps the log of the whole suite for the test secrets | step 3 onwards |
| T33 | The setup lives in clicks, or in scripts run inside the server; two instances cannot be made alike; a restore brings back yesterday's settings | Nexus (the web interface, Groovy scripts), Harbor; memory | O1: a file, validated, reloaded; the pages are read-only | step 1 |
| T34 | Eight services, a queue, a cache and an installer to run one registry; an upgrade is a migration by a tool | Harbor, Pulp, Artifactory; memory | one program, one database, blobgate; D4 | step 1, step 12 |
| T35 | A heap to size by hand, gigabytes at rest | Nexus, Artifactory (Java); memory | N1: a figure for memory that is measured and kept by a test | step 4, step 12 |
| T36 | Leaving is hard: properties, builds and promotions are in a format of the product; export is a paid tool or a script per format | Artifactory, Nexus; memory | O7: export to plain layouts, tested by installing from them; builds are in-toto statements | step 10 |
| T37 | A reverse proxy in front cuts large uploads or buffers them to its disk | all; memory | O3: none needed; the page says the three settings | step 6 (the reverse proxy leg, with a 5 GiB push) |

### Traps of our own base

| # | Trap | The way round | Proved in |
|---|---|---|---|
| T38 | rqlite has one writer; a registry of millions of components may ask more of it than blobgate does | D2: measure in step 2, before any format is built on it; the database behind one package | step 2 |
| T39 | blobgate holds an 8 MiB block per read: many slow pulls of large layers were feared to queue | **measured 2026-11-17 ([step0-probes.md](step0-probes.md)): they do not queue, they cost about 10 MiB of blobgate's memory each, kept (562 MiB with 50 readers, 5 GiB with 500).** Way round: godepot limits the downloads going on at once and ends a read slower than a minimum rate; the sizing says 10 MiB per open download; blobgate streaming a block (its roadmap item 2) removes the cost. The presigned redirect does not help | step 4 (a test with slow clients against godepot) |
| T40 | Many small files are slow on FTP and WebDAV nodes | S5: small documents are rows | step 2 |
| T41 | Writing a format for every ecosystem at once, none finished (the reason "universal" managers are large) | tiers; a format is released only when its script passes for hosted, proxy and group | step 6 |
| T42 | Two maintainers: more surface than can be kept safe | no plugins, no scripting, no page that writes; tier 3 only on request | the spec's "Does not do" |

## What is not known yet

- Whether rqlite reaches N1 (T38). Step 2 answers.
- Whether `?link` of a 10 GiB upload is quick enough to answer an OCI client within its timeout (step 0).
- How the importers behave on a Nexus or an Artifactory of real size; step 10 runs them on small, scripted ones only.
- The statements marked "memory" above.

## Sources read on 2026-11-16

- Sonatype: [tiers and feature availability](https://help.sonatype.com/en/nexus-repository-tiers-and-feature-availability.html), [feature matrix](https://help.sonatype.com/en/nexus-repository-feature-matrix.html), [cleanup policies](https://help.sonatype.com/repomanager3/nexus-repository-administration/repository-management/cleanup-policies), forum: ["Compact Blob Store" does not reclaim disk space](https://community.sonatype.com/t/task-compact-blob-store-does-not-reclaim-disk-space/10058), [Docker layer blobs no longer reclaimed](https://community.sonatype.com/t/docker-layer-blobs-no-longer-reclaimed-after-3-91-0-07-upgrade/16322)
- Harbor: [garbage collection](https://goharbor.io/docs/edge/administration/garbage-collection/), [the same for 2.0](https://goharbor.io/docs/2.0.0/administration/garbage-collection/), [issue 18014](https://github.com/goharbor/harbor/issues/18014)
- Cooldown: [Cloudsmith cooldown policy](https://docs.cloudsmith.com/supply-chain-security/epm/cooldown-policy), [Socket: dependency cooldown](https://socket.dev/glossary/dependency-cooldown)

These were read as search summaries, not page by page.
