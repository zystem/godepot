# The format go

Reference. The module proxy protocol of the `go` command (`GOPROXY`), for hosted, proxy and group repositories of the format `go`: every request, how a version is published, what is checked, what is cached for how long. How a proxy and a group behave in general (time to live, stale answers, pauses, claims) is in [proxy.md](proxy.md); this page says only what is particular to Go. The settings are in [settings-file.md](settings-file.md). Tested by `internal/proxytest` (a false module proxy) and `internal/gomod`, and run with the real `go` command by `tools/test/go-run.sh` ([testing.md](testing.md)).

A `go` repository is a `raw` repository whose paths are the paths of the protocol: the address of a file is `/repository/<repository>/<module>/@v/<version>.zip`. A module is the **name** that groups and claims work with.

## Use it

```bash
export GOPROXY=http://HOST/repository/<repository>
go get example.com/hello@v1.0.0
```

The `go` command sends **no credentials over plain HTTP** (it says `refusing to pass credentials to insecure URL`), so a repository that the `go` command reads over HTTP is one with `anonymous_read: true`; with TLS in front (a reverse proxy: [oci.md](oci.md) says why the program has none yet) the credentials may be in the address (`https://user:TOKEN@HOST/repository/<repository>`). Publishing is not done by the `go` command: use `curl` with a token (below). **Not run:** the `go` command with credentials over TLS.

## The requests

| Request | Answer |
|---|---|
| `GET <module>/@v/list` | the versions, one to a line, in order of version (a hosted repository: those that have a zip). `404` for a module with none |
| `GET <module>/@v/<version>.info` | `{"Version": "v1.0.0", "Time": "2026-11-20T10:00:00Z"}`; for a hosted repository the time is that of the publish |
| `GET <module>/@v/<version>.mod` | the `go.mod` of the version |
| `GET <module>/@v/<version>.zip` | the module as a zip |
| `GET <module>/@latest` | the `.info` of the highest version: a release when there is one, else the highest prerelease |
| `PUT <module>/@v/<version>.zip` | hosted only: publish. `201`; `200` for the same bytes again; `409` for other bytes under a published version; `400` for a zip that fails the checks below; `413` over 500 MiB; `405` for a proxy or for a group with no `write_to` |
| `DELETE <module>/@v/<version>.zip` | hosted only: unpublish the version (its `.mod` and `.info` go too). `204`; `404` |

`HEAD` works as `GET`. **The module and the version are written as the protocol writes them:** a capital letter is `!` and the small letter (`github.com/Azure/x` is `github.com/!azure/x`); a bare capital, or a `!` before anything else, is `404`. Any other path is `404`: a Go repository, a proxy included, is not a way to fetch anything under its upstream. The path of a module is up to 512 bytes with the characters `a-z 0-9 ! . _ ~ + -` and `/`, no `..`, no element that begins with `-`; a version is `vMAJOR.MINOR.PATCH` with an optional `-prerelease` and `+incompatible` (the other build metadata is not taken). Not answered: `/sumdb/...` is `404`, and the `go` command then asks the checksum database itself (run: see below).

## Publish

`PUT` the zip as the `go` command makes it (`example.com/hello@v1.0.0/go.mod`, ...). It is read where it is kept, never unpacked to a disk (docs/spec.md, F5), and refused with `400` unless:

- it is a zip, with at most 20 000 files, whose files add up (as the zip says) to at most 500 MiB;
- every name begins with `<module>@<version>/`, has no `..` or `.` part, no backslash, no doubled slash, and is not in the zip twice;
- it has `<module>@<version>/go.mod`, of at most 16 MiB, whose `module` line is the module of the address;
- the version goes with the path: `v2` and later need a `/v2` (`/vN`) suffix on the module, `v0` and `v1` none, `+incompatible` none.

Then the `.mod` is made from the `go.mod` of the zip and the `.info` from the version and the time, and the three are written; the version is in `list` when its zip is there. **A published version is never overwritten** (`409`; R4). The `go.sum` hash of a module is that of its zip, so a version whose bytes could change would break every build that has it. A claim (R3) on the module is written in the same transaction as the zip: **after a module has been published to a repository in a group, its name is never asked of the group's proxies**, also after it is unpublished.

```bash
curl -X PUT -H "Authorization: Bearer $TOKEN" --data-binary @hello.zip http://HOST/repository/<repository>/example.com/hello/@v/v1.0.0.zip
```

## A proxy

A proxy asks the upstream (`proxy.golang.org` or another module proxy) for exactly the paths above, by the same rules as [proxy.md](proxy.md), with these particulars:

| What | Rule |
|---|---|
| `.info`, `.mod`, `.zip` of a version | fetched once, kept, and never asked again (a version cannot change) |
| `list` and `@latest` | trusted for `ttl` (default `5m`), then asked again; **served stale, with `Warning: 110`, when the upstream cannot answer** |
| a module the upstream lacks | `404`, remembered for `negative_ttl` (default `10m`) |

**Not done:** the checksum database is not proxied (`/sumdb/` is `404`; the `go` command uses `sum.golang.org` directly, which is run); a module that the upstream shows only as a pseudo-version of a branch (`@v/master.info`) is `404`.

## A group

A group answers for a module from **the first member that has it** (R2): a hosted member that holds a file of the module, or a proxy whose cache holds one or whose upstream lists it. The versions of one module are never mixed from two members: `list` is that of one member. A prefix in `claims` (`corp.example.com/`) and every module published to a hosted member are claimed (R3): the group does not ask a proxy for them. This is the guard against a public module taking the name of a private one (T12). The `GOPRIVATE` of the `go` command is still the way to keep a name away from the checksum database.

## Checked

| What | How | Date |
|---|---|---|
| publish, the same bytes, other bytes, list order (`v1.10.0` after `v1.2.0`), latest, unpublish, the refusals above (a name outside the module, `..`, a `go.mod` of another module, a v2 without `/v2`, a zip that is not a zip) | `internal/proxytest` (`go_test.go`), each check broken on purpose to see its test fail | 2026-11-20 |
| a proxy: a version fetched once, `list` after its time, stale when the upstream is down, a missing module remembered; a group: our module not asked of the public proxy, claims, `write_to` | the same, with a false module proxy | 2026-11-20 |
| the real `go` command: a module zipped, published through the group and built by `go run`; `github.com/google/uuid` through a proxy of `proxy.golang.org` with the real checksum database, again with an empty module cache (no request upstream); a claimed name not found and not asked | `tools/test/go-run.sh`, Go 1.26.11 | 2026-11-20 |

**Not run:** the `go` command with credentials over TLS; `GOFLAGS=-mod=vendor` and private modules with `GOPRIVATE`; a module larger than a few KiB; `go get` of a branch name or pseudo-version through a hosted repository (a hosted repository has only the versions that were published).
