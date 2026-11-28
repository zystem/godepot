# Proxy and group repositories

Reference. What a `proxy` repository and a `group` repository do, for the formats `raw` and `oci`: each rule, its default, what is remembered and for how long, what the client sees, what the audit log and the metrics say. The words of the settings file are in [settings-file.md](settings-file.md); how to set one up is in [proxy-howto.md](proxy-howto.md). Why the rules are these (the traps of other products, the requirements P1 to P9 and R1 to R3) is in [spec.md](spec.md) and [plan.md](plan.md). Tested by `internal/proxytest` (false upstreams) and by `tools/test/proxy-run.sh` (the program against a real blobgate, a real rqlite, Docker Hub and a web server): [testing.md](testing.md) says what was run.

## A proxy

A **proxy** repository copies, on demand, from one upstream. It is **read-only**: a `PUT`, a `POST` or a `DELETE` to it is `405`. Nothing is copied until a client asks for it.

| What is asked | Where the answer comes from |
|---|---|
| a file of a proxy of the format `raw` | the cache, when the file is there and trusted (see `ttl`); otherwise the upstream |
| an OCI blob | the cache when the name has it; otherwise the upstream |
| an OCI manifest **by digest** | the cache; otherwise the upstream |
| an OCI manifest **by tag** | the cache for `ttl` (default `5m`); after that, a `HEAD` to the upstream says whether the tag still names the same manifest: if so the cache goes on, if not the new manifest is fetched; the tag then names it |
| the list of tags | the upstream's own list (with `n` and `last` passed on); from the cache, marked stale, when the upstream cannot answer |
| referrers | the cache only: the referrers API of the upstream is not asked |
| the listing of a raw proxy (a path that ends with `/`) | `404`: an upstream has no listing to give |

**Streaming.** The first byte goes to the client as soon as the upstream sends it, while the same bytes are written to the store (P1; a 5 GiB layer is measured below). The client's body is **one byte behind** until the bytes have been checked: if the upstream named a digest and the bytes do not have it, the connection is cut one byte short, so a client that counts the body sees the failure, and nothing is kept. For an OCI blob the digest is the one in the address; for a raw file it is `X-Checksum-Sha256` when the upstream sends one.

**Joined fetches (P2).** Requests for the same missing thing are joined: one fetch from the upstream, and the others wait for it and then read from the cache. The joining is within one replica; two replicas asked at once each fetch once. A client that waits gets the bytes only when the fetch has ended (the one that started it is streamed). The fetch is **not tied to the client that started it**: if that client goes away the fetch goes on, for the cache and for the others, for at most 2 hours.

**What cannot change is asked once (P3, P5).** A blob and a manifest by digest are named by their digest: once cached they are never asked again. Another body under such a digest is **not kept**; the client is cut short, the audit log has an `upstream_changed` line (result `refused`), and `godepot_upstream_changed_total` counts it. For a raw proxy, `immutable_paths` says which files cannot change: after `ttl` the file is read once more, and when its bytes differ the cached bytes stay and the same alarm is raised. Without `ttl` a raw file is never asked again, so nothing can change.

**What can change has a time to live (P3).** An OCI tag has `ttl` (default `5m`); a raw file has none unless `ttl` is set. After it, the upstream is asked with `If-None-Match` (raw) or with a `HEAD` (an OCI tag, which costs the upstream's rate limit less than a `GET`). **When the upstream cannot answer, the cached copy is served** with `Warning: 110 godepot "Response is stale"` and `godepot_upstream_stale_total` counts it. What was never cached is `503`.

**A "not found" is remembered for a short time, an error never is (P4).** A `404` from the upstream is remembered per name for `negative_ttl` (default `10m`; keys: `raw:<path>`, `manifest:<image>@<reference>`, `blob:<image>@<digest>`, `name:<image>`) and answered `404` meanwhile. An error, a timeout, a `429` or a `5xx` of the upstream is **never** remembered as "not found": it **pauses the upstream** (and only that one): 5 seconds after the first error, twice as long after each next in a row, at most 5 minutes; a `Retry-After` longer than that pause is used, also to the most of 5 minutes. During a pause nothing is sent to the upstream: a request is answered from the cache when it can be (stale), else `503` with `Retry-After`. One good answer ends the run. `POST /api/v1/repositories/<name>/forget` clears the remembered misses (all, or `?name=<key>`) and, with no `?name=`, the pause ([api.md](api.md)). The pause and the misses are in the database, so every replica sees them.

**Credentials and trust (P8).** The secret is never in the settings file: it is given with the API and kept sealed in the database ([secrets.md](secrets.md)), or `password_env` and `token_env` name environment variables (`NAME`, or a file named by `NAME_FILE`). A new secret is used with no restart (from the database: within 5 seconds on the other replicas). They are sent to the upstream and nowhere else: not after a redirect to another host (Docker Hub redirects a layer to a CDN; the CDN is sent no `Authorization`), and not to a token service on another host unless it is in `token_hosts`. A registry that answers `401` with `WWW-Authenticate: Bearer realm=...` is spoken to as the distribution spec says: a token is fetched from the realm (anonymous unless the host is trusted), kept for the time it says (at most 5 minutes), and sent. An upstream that refuses the credentials is `502`, and pauses the upstream like an error.

**Address (P9).** The address of the upstream is the operator's, in the settings; nothing in a request chooses it. After the name is resolved, **a loopback, private, link-local (the address of a cloud's metadata service is one), unspecified, multicast or shared (`100.64.0.0/10`) address is refused**, for the upstream, for each redirect and for the token service, unless `allow_private` is `true`; the refusal is `502` and is not an error of the upstream (no pause). The program takes no proxy from the environment (`HTTP_PROXY` is not read): the check must see the address that is dialled. A response is never decompressed: bytes are kept as they were sent.

**Limits.** `max_concurrent` (default 16) and `max_per_second` (default none) bound the requests to one upstream. The downloads to clients are limited as everywhere (`GODEPOT_MAX_DOWNLOADS`, `GODEPOT_MIN_DOWNLOAD_RATE`: [configuration.md](configuration.md)).

### The words a client sees

| Situation | Format `raw` | Format `oci` |
|---|---|---|
| a write to a proxy | `405` | `405` `UNSUPPORTED` |
| the upstream has no such thing | `404` | `404` `MANIFEST_UNKNOWN`, `BLOB_UNKNOWN`, `NAME_UNKNOWN` |
| the upstream is paused or failed, nothing cached | `503` and `Retry-After` | `503` `UNAVAILABLE` and `Retry-After` |
| the upstream refused the credentials, or its address is private, or it gave other bytes than its digest | `502` | `502` `UNAVAILABLE` |
| too many downloads at once | `503`, `Retry-After: 5` | `503` `UNAVAILABLE`, `Retry-After: 5` |
| the body was cut: the upstream died in it, or the bytes did not have the digest | the connection is cut; nothing is kept; a dead upstream is paused | the same |

### What is written

A fetch writes the cached thing in one transaction with its references (so the collector never frees what the cache names) and one line of the audit log: action `fetch`, actor `proxy`, the repository, the path (`<image>/manifests/<digest>`, `<image>/blobs/<digest>`, or the raw path) and the digest. A proxied raw file is a component made by `proxy`. The cached blobs are the same blobs of the store as everything else: bytes that a proxy and a hosted repository both have are kept once. When bytes are refused because their digest is on the block list ([policy.md](policy.md)) the line is `blocked` (result `refused`). **Not built yet:** removing from a proxy's cache (cleanup comes with the policies of step 7: until then the cache only grows), and a size limit of a cache.

**Cooldown and block lists (P6, P7).** A proxy of the formats `npm`, `pypi` and `go` can hold back the versions that the upstream published lately, and any proxy but `oci` can refuse versions and digests by name: [policy.md](policy.md).

## A group

A **group** answers from its members, one address for hosted and proxy repositories together (R1, T20). The settings: `members` (in order), `write_to`, `claims`.

**A read** is answered by one member, the **first that has the name** (R2). The name is the whole path for `raw`, and the image (the name without the repository) for `oci`: all the tags and blobs of an image come from the same member, so **the versions of one name are never mixed** from a hosted and a proxy member (`merge: true` would; it is refused). A member that is a group is replaced by its own members, once each.

How a member "has" a name: a hosted repository when it holds it; a proxy when its cache holds it or, if not, when its upstream says so (a `HEAD` of the file, a list of tags of the image with `n=1`; a "not found" is remembered like any other). **An error of the upstream while asking is an error of the request** (`503`): the next member is *not* asked in its place, because the answer of a later member would stand in for the one that cannot answer.

**Claims (R3, T12).** This is the guard against dependency confusion. In a group:

1. A hosted member that has the name answers, **whatever its place in the list**: a proxy that comes first is not asked.
2. A name that the group **claims** is `404` when no hosted member has it, and no proxy is asked. A name is claimed when a prefix of `claims` begins it (of the group, or of a group inside it), or when it was **published** to a hosted member of the group: the claim is written in the transaction that publishes the name (a `PUT` of a raw file, a push of an OCI manifest) and **stays after the name is deleted**, so deleting your package does not give its name to the upstream. A claim that was never written (a name published before step 5) is not known: list the prefix in `claims`.
3. Then the proxies in order.

**A write** to a group (`PUT`, `DELETE`, a push, an upload) goes to the member `write_to` names; with none, it is `405`. The request is authorized against the **group** (a grant on the group is what a client of it needs); the upload's `Location` and the `Link` of a tags list name the group, so the client stays on it. In the audit log the repository is the member that did the work.

**A raw listing** (a path that ends with `/`) is the listing of the first hosted member that has files under it, and empty when there are none; a proxy contributes nothing to it.

## Checked

| What | How | Date |
|---|---|---|
| each rule above: fetch once, joined fetches (100 clients, 1 fetch), streaming before the end, an upstream that dies in the body, `429` and `500` pauses growing to the cap, a "not found" remembered and cleared, stale served, changed bytes kept out, other bytes under a digest, a private address, credentials not sent to another host, the token service, groups, claims, `write_to`, a member that is down | `internal/proxytest`, false upstreams of the test; each is broken on purpose (the guard taken out) to see its test fail | 2026-11-20 |
| the program, a real rqlite and blobgate 0.6.1: a big layer, 100 clients, Docker Hub, a group, a raw proxy | `tools/test/proxy-run.sh` | see [testing.md](testing.md) |

**Not run:** two replicas at once (the joining is per replica, by design); an upstream that is a real Artifactory or Nexus; a proxy of a registry that needs `token_hosts` with credentials (Docker Hub with a paid account); `docker` and `podman` pulling through a proxy (they were not run against godepot at all: [oci-clients.md](oci-clients.md)).
