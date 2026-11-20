# The format oci

Reference. Every request of the OCI door as it is in this release (run 2026-11-20). It speaks the OCI distribution API 1.1: images, indexes, Helm charts, signatures and any other artifact. How to connect `docker`, `helm`, `oras`, `cosign` and the others is in [oci-clients.md](oci-clients.md); why the pieces are shaped so is in [blob-layer.md](blob-layer.md) and [spec.md](spec.md) (F1, C5, R4).

## The address

```
http://HOST:PORT/v2/<repository>/<image>/<what>
```

- The **name** of an image is `<repository>/<image>`. The first part is a repository of the [settings file](settings-file.md) with `"format": "oci"`; the rest is the image and may have several parts (`team/app/base`). All repositories share one port and one host name (spec F1): there is no port and no host name per repository.
- A part of a name is lower-case letters and digits, separated by `.`, `_`, `__` or runs of `-`. The whole name is at most 255 bytes. A name with no image (`/v2/reg/tags/list`) is `400 NAME_INVALID`. A first part that is no repository of the format oci is `404 NAME_UNKNOWN`.
- `<what>` is one of the requests below. The words `blobs`, `manifests`, `tags` and `referrers` are looked for from the right, so an image may be called `tags`.
- A **digest** is `sha256:` and 64 lower-case hex digits. Other algorithms are refused (`400 DIGEST_INVALID`). A **tag** is up to 128 of `a-z A-Z 0-9 _ . -`, not beginning with `.` or `-`.
- The **path** that a grant matches ([settings-file.md](settings-file.md)) is the image (`team/app`), without the repository.

## Who is let in

`GET /v2/` is the check that every client makes first. With no token it is `401` and the header `WWW-Authenticate: Basic realm="godepot"`; with a good token it is `200` and `{}`. A token is sent as the **password of basic authentication** (any user name; the account is the token's own) or as `Authorization: Bearer <token>` ([access.md](access.md)). The answers of a refusal are in the words of the API: `{"errors": [{"code": "...", "message": "...", "detail": ...}]}`.

| Request | Action it needs |
|---|---|
| `GET`, `HEAD` of a blob, a manifest, the tags, the referrers; `GET` of the state of an upload | `read` |
| `POST`, `PATCH`, `PUT` of an upload; `PUT` of a manifest; `DELETE` of an upload | `write` |
| `DELETE` of a manifest or a tag | `delete` |

A request with no token may read from a repository with `anonymous_read`, and nothing else. A mount of a blob also needs `read` on the image it is taken from.

## The requests

| Request | Answers |
|---|---|
| `GET /v2/` | `200`; `401` |
| `GET`, `HEAD` `/v2/<name>/blobs/<digest>` | `200` with `Docker-Content-Digest`, `Content-Length`, `Accept-Ranges: bytes`; `206` for a `Range` of one range (`Content-Range`); `400 DIGEST_INVALID`; `404 BLOB_UNKNOWN` (the name has no such blob, whoever else has it); `416`; `503 UNAVAILABLE` with `Retry-After` when too many downloads are going on |
| `DELETE /v2/<name>/blobs/<digest>` | `405 UNSUPPORTED`: a blob goes with the cleanup of the repository, when no manifest names it (not built yet: [Not done](#not-done)) |
| `POST /v2/<name>/blobs/uploads/` | `202` and the address of the upload (`Location`, `Docker-Upload-UUID`, `Range: 0-0`); with `?digest=<digest>` and the blob as the body: `201`; with `?mount=<digest>&from=<name>`: `201` when the blob is taken from the other name, else `202` as with no mount |
| `GET /v2/<name>/blobs/uploads/<id>` | `204` with `Range: 0-<last byte>`; `404 BLOB_UPLOAD_UNKNOWN` |
| `PATCH /v2/<name>/blobs/uploads/<id>` | `202` with the new `Range`; `416 BLOB_UPLOAD_INVALID` when `Content-Range` does not begin where the upload ends, or when another request added a part at the same time; `404 BLOB_UPLOAD_UNKNOWN` |
| `PUT /v2/<name>/blobs/uploads/<id>?digest=<digest>` | `201` with `Location` and `Docker-Content-Digest`; the body, if any, is the last part. `400 DIGEST_INVALID` when the bytes of all the parts are not the digest named (the upload stays, for the client to `DELETE`); `404 BLOB_UPLOAD_UNKNOWN` |
| `DELETE /v2/<name>/blobs/uploads/<id>` | `204`; `404 BLOB_UPLOAD_UNKNOWN` |
| `GET`, `HEAD` `/v2/<name>/manifests/<tag or digest>` | `200` with `Content-Type` (the media type it was pushed with), `Docker-Content-Digest`, `ETag`; `404 MANIFEST_UNKNOWN` (also for a reference that is neither a tag nor a digest: nothing can be there under it) |
| `PUT /v2/<name>/manifests/<tag or digest>` | `201` with `Location` and `Docker-Content-Digest` (and `OCI-Subject` when the manifest names a subject); `400 MANIFEST_INVALID`, `400 MANIFEST_BLOB_UNKNOWN` (it names a blob or a manifest that the name does not have; `detail` says which), `400 DIGEST_INVALID` (a digest in the address that is not the manifest's), `400 TAG_INVALID`; `409 DENIED` (the tag is in `immutable_tags` and names another manifest); `413 MANIFEST_INVALID` (over 4 MiB) |
| `DELETE /v2/<name>/manifests/<tag>` | `202`: the tag goes and nothing else; `404 MANIFEST_UNKNOWN` |
| `DELETE /v2/<name>/manifests/<digest>` | `202`: the manifest and the tags that name it go; `404 MANIFEST_UNKNOWN`; `409 DENIED` when an index of the name lists it |
| `GET /v2/<name>/tags/list` | `200` `{"name": ..., "tags": [...]}` in order; `?n=` and `?last=` page it, and a `Link: <...>; rel="next"` says there is more; `n=0` is an empty list; `400 PAGINATION_NUMBER_INVALID`; `404 NAME_UNKNOWN` for a name that has nothing |
| `GET /v2/<name>/referrers/<digest>` | `200` an image index (`application/vnd.oci.image.index.v1+json`) of the manifests whose `subject` is the digest, each with `artifactType` and `annotations`; `?artifactType=` filters, and `OCI-Filters-Applied: artifactType` says it did; the subject need not be there; `400 DIGEST_INVALID` |

Any request may also get `401`, `403` (a token that may not do the action here: `DENIED`), `503 UNAVAILABLE` (the database or the store cannot be reached: try again) or `502 UNAVAILABLE` (the store failed). The other codes of the specification are not used: `BLOB_UNKNOWN` and the rest above are all there is, with `NAME_UNKNOWN`, `NAME_INVALID`, `UNAUTHORIZED` and `UNSUPPORTED`.

### Uploads

- **One request** (`POST ...?digest=`) and **two requests** (`POST`, then `PATCH` with the whole body, then `PUT ?digest=`, which is what `docker` does) write the bytes once, as they come: the SHA-256 is computed on the way and the blob is made at the end of the `PATCH`; the `PUT` only links it to the name. Nothing is written to the disk of godepot.
- **Parts** (several `PATCH`): each part is taken in as a blob of its own, held by the upload. At the `PUT` the parts are read in order and written as one blob, and the digest of the whole is checked; **this reads and writes the layer once more**, so a client that can should send the layer in one `PATCH` (a 512 MiB layer in parts of 64 MiB took 3.3 seconds more at the `PUT`; one `PATCH` takes no time more). A `PUT` of one part only needs no second write.
- An upload that is not ended within `GODEPOT_UPLOAD_TTL_SECONDS` (24 hours by default) is swept by the replica that leads; its parts are let go and collected after `GODEPOT_GRACE_SECONDS`.
- A blob is visible to a name only after it has been pushed to it or mounted into it. A name that does not have a blob is `404` for it, even when the same bytes are in godepot for another name: a digest is not a key to every repository.

### Mount

`POST /v2/<name>/blobs/uploads/?mount=<digest>&from=<other name>` takes a blob that the other name has, without sending it, **when the other name is in the same repository** (the first part is the same) and the account may read it. In every other case an upload is started, as the specification says. A mount writes one row and one reference.

### Manifests

- The media types taken: `application/vnd.oci.image.manifest.v1+json`, `application/vnd.oci.image.index.v1+json`, `application/vnd.docker.distribution.manifest.v2+json`, `application/vnd.docker.distribution.manifest.list.v2+json`. The `Content-Type` of the request must be one of them and, when the body has a `mediaType`, the same. Docker's schema 1 is not taken.
- The body must be JSON with `schemaVersion` 2. An image manifest names a `config` and `layers`; every one of them must be a blob **of this name** (layers of the media types `...foreign...` and `...nondistributable...` are kept elsewhere and are not checked). An index names manifests, and every one must be a manifest of this name. The check and the write are **one transaction**: a blob cannot go between them.
- A manifest is kept as it came, byte for byte, so its digest is the one the client computed. Up to 256 KiB it is a row of the database; above that, up to 4 MiB, it is a blob.
- A **tag** moves when it is pushed again with another manifest, unless the repository lists it in `immutable_tags`: then pushing another manifest under it is `409 DENIED` and audited. The same manifest again is `201` and changes nothing.
- `artifactType` of a manifest is its `artifactType`, or the `mediaType` of its config when it has none. A manifest with a `subject` is listed by the referrers API of the subject. The API is always there: no client needs the fallback of a tag named after the digest.
- **Deleting.** A tag is removed alone. A manifest is removed with the tags that name it, but **not while an index of the same name lists it** (`409`: delete the index first), so that no index is left with a child it cannot find. A manifest that names a subject and the manifests that name it as subject are **not** removed with it: a signature stays listed after the image it signs is deleted, for the cleanup of step 7 to judge. The blobs a manifest names stay with the name; unnamed bytes are collected after the grace time once nothing holds them.

### The tables

A blob of a name is a row of `oci_blobs` and one reference of the blob layer, owned by `ociblob:<repository>/<image>@sha256:<hex>`; a manifest is a row of `oci_manifests`, its tags are rows of `oci_tags`, what it needs is rows of `oci_links` (`blob` or `child`); an upload is `oci_uploads` with `oci_parts`. The schema is step 4 of `internal/meta/schema.go`.

## Audit and metrics

Every write and delete is a line of the audit log, in the transaction of the change ([access.md](access.md)): action `put` or `delete`, repository, path `<image>/blobs/sha256:<hex>` or `<image>/manifests/<reference>`, and as detail the digest, the size and the media type (`mounted from <name>` for a mount). Refusals are lines with the result `refused`. Metrics: `godepot_reads_total` (a blob or a manifest that was sent), `godepot_writes_total`, `godepot_deletes_total`, `godepot_refused_total{reason}` with `busy`, `conflict`, `digest` and the access reasons, `godepot_downloads_active` ([metrics.md](metrics.md)).

## Downloads at once

The blobs being sent at once are limited by `GODEPOT_MAX_DOWNLOADS` (64 by default; every download holds about 10 MiB of blobgate's memory, [step0-probes.md](step0-probes.md)). A download over the limit waits up to 5 seconds, then gets `503` with `Retry-After: 5`. A client that takes less than `GODEPOT_MIN_DOWNLOAD_RATE` bytes a second (8192 by default; 0 for no least) is ended: each write must be taken within ten seconds plus the time its size takes at that speed. The limit is shared with the format `raw`.

## Not done

- **Deleting a blob** (`405`); blobs go with a cleanup that is step 7. Until then the layers of deleted images stay.
- **Mounting from another repository**, and the automatic mount when `from` is left out (an upload is started instead).
- **`/v2/_catalog`** (`404`: no repository is called `_catalog`).
- **Quotas** for an OCI repository: the file is refused when it sets one.
- Proxy and group repositories (step 5); TLS on the port of the program (the OCI clients refuse plain HTTP, except `localhost` for some, so put a proxy that serves TLS in front, or add the registry to the insecure ones of the client).
- Docker's manifests of schema 1, and digests other than SHA-256.

## What was run

On 2026-11-20, godepot built from the branch of step 4, a real rqlite and a real blobgate (the stand of [testing.md](testing.md), blobgate 0.6.1 as a program, two directory nodes):

- **The conformance suite** of the specification (v1.1.1, commit `a139cc4`), all four groups (pull, push, content discovery, content management): 73 passed, 0 failed, 6 skipped. The six skips are decisions of the suite itself: the tag taken from the environment and the setup that fills the registry without pushing (`OCI_TAG_NAME` and `OCI_TAG_LIST` are not set), the two tests of a mount with no `from` (they run only when `OCI_AUTOMATIC_CROSSMOUNT` is set; this registry does not mount by itself), the test of a mount that is not possible (it runs only after a mount that answered `202`, and the mount of the suite answered `201`), and the read of a blob after its deletion (it runs only after a `DELETE` of a blob that answered `202`; here it is `405`).
- **Clients**: `crane` 0.20.6 (copy of `alpine:3.20`, a multi-platform image with attestations, in; the digest of the tag is the one at the source), `skopeo` 1.13.3 (copy of `alpine:3.19` in and out), `helm` 3.20.0 (a chart pushed and pulled, the file equal), `oras` 1.2.3 (an artifact pushed, a note attached and found by `discover`, pulled), `cosign` 3.0.2 (an image signed with a key and verified; the signature is a referrer). `tools/test/oci-run.sh` does all of it.
- **A 10 GiB layer**, one `PATCH` (`tools/ocibig`): pushed in 60.2 seconds, pulled in 25.5 seconds with the digest equal, a range from the middle equal; the resident memory of the program was 16 MiB before and 17 MiB at the peak after (`VmHWM`). A layer of 512 MiB in parts of 64 MiB: the same bytes, the `PUT` took 3.3 seconds.
- **Slow clients**: 20 clients that read 4 KiB of a 10 GiB layer and then nothing, against `GODEPOT_MAX_DOWNLOADS=8`: 8 downloads open, 12 answered `503` after the 5 seconds they waited, and the 8 that read nothing were ended by the program within 20 seconds (`godepot_downloads_active` went from 8 to 0). The memory of the program did not move.

**Not run**: `docker` (the daemon of this machine does not take a registry on plain HTTP and its settings are the owner's), `podman` (not installed), `containerd`, `kubelet` pulling an image; a layer of 10 GiB in parts; two replicas at once; a slow node of blobgate; the memory of blobgate itself during the 10 GiB push (not looked at). The README try-it does not show the OCI door yet.
