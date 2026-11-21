# The format raw

Reference. Files by path, as they are: no names to understand, no index to make. `curl` is the client. Served at `/repository/<repository>/<path>` for a repository of `"format": "raw"` ([settings-file.md](settings-file.md)). Checked by tests that run the whole program against a real rqlite and a real blobgate (`cmd/godepot/raw_test.go`, `internal/server`), and by the try-it of the README.

Every request needs a token ([api.md](api.md)), as a bearer token (`Authorization: Bearer <token>`) or as the password of basic authentication (any user name; `curl -u x:<token>`), unless the repository has `anonymous_read` and the request only reads. A token is read from that header **only**, never from the address. Without one the answer is `401` with `WWW-Authenticate: Basic`; with a token that may not do this, `403`. The answers carry a JSON body `{"error": "..."}` and sometimes `"info"`.

## The path

1 to 1024 bytes of UTF-8, parts separated by `/`. No empty part (`a//b`), no `.` or `..` part, no control character, no backslash, and a file path does not end with `/`. Anything else is `400`. Paths are case sensitive.

## Write: `PUT /repository/<repository>/<path>`

Needs `write` on the path. The body is the file, of any length (it is read as a stream: godepot does not hold it in memory or write it to a disk).

| Header | Meaning |
|---|---|
| `Content-Type` | kept, up to 255 bytes, and sent back on reads |
| `X-Checksum-Sha256` | the SHA-256 of the body in hex (any case). When the bytes do not have it, nothing is kept and the answer is `400`. When the path holds other bytes and the repository does not overwrite it, the answer is `409` **before** the body is read |
| `X-Godepot-Property-<Name>` | a property of the file (`X-Godepot-Property-Build: 42` sets `build`). The name is 1 to 64 of `a-z 0-9 . _ -` (any case in the header), the value up to 1024 bytes, at most 32 properties. Sent back on reads, filterable in listings |

| Answer | When |
|---|---|
| `201` | a new file. The body is `{"path": ..., "sha256": ..., "size": ...}`; `ETag` and `X-Checksum-Sha256` carry the digest |
| `200` | the same bytes were there already (nothing changed, the audit line says `unchanged`), or the path may be overwritten and now holds the new bytes |
| `400` | a bad path, a bad header, bytes that are not the digest named |
| `409` | the path holds **other bytes** and is not in `overwrite_paths`: a file is immutable (spec R4). The body names what is there |
| `413` | the repository would go over `quota_bytes` or `quota_components`; the body has the figures. Nothing is kept |
| `503` | the database or the store is not available: send it again. A write interrupted at any point leaves no file that is half there: the bytes of one that never finished are removed after `GODEPOT_UPLOAD_TTL_SECONDS` |

Writers of one path at once: one wins (`201`), the others get `409`, or `200` when they sent the same bytes. In a path that may be overwritten the last to commit stands and the bytes of the others are unnamed.

## Read: `GET` and `HEAD /repository/<repository>/<path>`

Needs `read`. Sends the bytes, with `Content-Type` (`application/octet-stream` when none was given), `Content-Length`, `ETag: "<sha256>"`, `X-Checksum-Sha256`, `Accept-Ranges: bytes` and the properties. `If-None-Match` with the ETag is `304`.

A file is never run as a page by a browser: the answer has `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox`.

`Range: bytes=a-b`, `a-` or `-n` is `206` with `Content-Range`; a range outside the file is `416`. Several ranges are answered with the whole file (the protocol allows it). `404` when there is no such file. `500` with a message when the file is there and its bytes are not in the store: that is lost data, and `godepot reconcile` lists it.

## List: `GET /repository/<repository>/<directory>/`

A path that ends with `/` (or is empty: the root) lists the files **below** it, all depths, in order of path. Needs `read` on the directory, as a path with the `/` at its end: a token that is allowed `releases/**` may list `releases/` and not the root.

| Query | Default | Meaning |
|---|---|---|
| `limit` | `1000` | 1 to 10000 |
| `after` | none | the path to continue after (the `next` of the last answer) |
| `property=name:value` | none | only the files whose property has that value |

The answer is `{"items": [{"path", "size", "sha256", "content_type", "created_at", "properties"}], "next": "<path>"}`; `next` is there when there is more. `created_at` is seconds since 1970.

## Delete: `DELETE /repository/<repository>/<path>`

Needs `delete`. `204`, or `404` when there is no such file. The file is gone at once and **goes to the trash** (`GODEPOT_TRASH_DAYS`, default 7), where it can be put back whole and still holds its bytes ([cleanup.md](cleanup.md)); the audit log has a line `remove`. When it is emptied (after the days, or by hand) its bytes stay for `GODEPOT_GRACE_SECONDS` (24 hours by default), as they may be named by another file or by a push in progress, and are then removed by the collector ([blob-layer.md](blob-layer.md)). With `GODEPOT_TRASH_DAYS=0` there is no trash and the audit line is `delete`.

## What is written

Every write, every delete and every refusal is a line of the audit log ([api.md](api.md)); a change and its line are written in one transaction, so there is no change without its line. Reads are counted ([metrics.md](metrics.md)) and not audited.

## Proxy and group

A `raw` repository may be a `proxy` (a copy, on demand, of the files of a web server: the first `GET` fetches and keeps, the next ones do not ask) or a `group` (hosted and proxy repositories under one name; a `PUT` to it goes to `write_to`). The requests are the same; a `PUT` or a `DELETE` of a proxy is `405`, and a listing of a proxy is `404`. Everything else (time to live, stale answers, what is remembered as missing, the guard against a name of yours being served from an upstream) is in [proxy.md](proxy.md).

## Not there yet

Setting properties after the upload and searching across repositories (spec R6: at upload and in a listing only); a trash; promotion; redirects to presigned addresses; `HEAD` of a directory counts as a listing.
