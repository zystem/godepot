# Cooldown and block lists of a proxy

Reference. What a proxy repository holds back before it serves a version (P6, P7 of [spec.md](spec.md)): a **cooldown**, an **allow list** that lifts it, and a **block list**. The words are `cooldown`, `allow` and `block` of the `upstream` of a proxy ([settings-file.md](settings-file.md)); how a proxy works in general is in [proxy.md](proxy.md). Tested by `internal/policy` and `internal/proxytest`, and with the real clients by `tools/test/policy-run.sh` ([testing.md](testing.md)).

```json
{"name": "npmjs", "format": "npm", "kind": "proxy",
 "upstream": {"url": "https://registry.npmjs.org", "cooldown": "168h", "allow": ["@corp/*"], "block": ["left-pad@1.3.0", "evil-*", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"]}}
```

## The entries

An entry of `allow` or `block` is one of:

- `name`: every version of the name;
- `name@version`: the versions that match the pattern (`left-pad@1.3.*`); the version follows the **last** `@`, so a scoped name is `@types/node@20.*`;
- `sha256:<hex>`: a digest, 64 hex digits in lower case (`block` only).

A name and a version are patterns: `*` is any run of characters but `/`, `**` any run. What the **name** is depends on the format:

| Format | Name | Version |
|---|---|---|
| `npm` | the package, with its scope (`@corp/lib`), as published | `1.2.3` |
| `pypi` | the project, **normalised** (`foo-bar`) | the version of the file name |
| `go` | the module path, not escaped (`github.com/Azure/x`) | `v1.2.3` |
| `maven` | `groupId:artifactId` (`org.apache.commons:commons-lang3`) | the version |
| `raw` | the path of the file (`secret/**`); no version | none |

## Cooldown (P6)

With `cooldown` set, a version that the upstream published **less than that long ago** is:

1. **left out** of what the repository serves as the list of versions: the packument of `npm`, the index of `pypi`, the list and `@latest` of `go`; so a client takes the newest version that is left (for `npm`, the tag `latest` moves to the highest version that is left, no prerelease if there is another);
2. **refused by name** with `403`, the words `<name>@<version> was published at <time>, less than the cooldown of <D> ago: it will be served from <time>` and a `Retry-After` in seconds. A client that asks for it by name (a lock file) sees why.

**The time of publication** is the upstream's own: `time` of the full packument (npm: with a cooldown the proxy asks the upstream for the **full** packument, which is larger than the abbreviated one), `upload-time` of the index (PyPI: each file has its own, so a version whose files were uploaded at different times is shown in parts), `Time` of the `.info` of a version (Go). **A version for which the upstream gives no time is held back**: the cooldown cannot be checked, and a check that cannot be made fails closed. To serve it, put it on `allow`. After `cooldown` is set, a packument or index that was cached before has no time until it is asked again (`ttl`, default 5 minutes): the versions are held back until then.

`allow` lifts the cooldown for the versions it names. **It does nothing else**: a version that `block` names stays refused.

**Go** has no list with times: to leave a young version out of `list`, the `.info` of each version is read (once: a version cannot change, so it is kept), the newest 300 versions of the module by version; an older version is not looked at for its time, only for the block list. The first `list` of a module with many versions makes up to 300 requests of the upstream, 8 at once.

**Not built:** a cooldown for `maven` (a listing names no time, and the `Last-Modified` of a file is not the time it was published), `raw` and `oci`. A version that is already cached is held back like any other: the lists and the by-name refusals are decided at every request.

## Block list (P7)

What `block` names is never served: **left out of the lists** (for `maven`, the `<version>` entries of `maven-metadata.xml`, and `latest` and `release` move to the highest that is left, and the checksum of the metadata is of what is served) and **refused by name** with `403` and `<name>@<version> is on the block list of this repository`. The refusal is made **before** the upstream is asked: a blocked version is never fetched.

A **digest** is known only when the bytes are in: for `pypi` it is in the index, so the file is left out of the index and refused by name; for the other formats the bytes are fetched, and when they have the digest they are **not kept** and the body is cut one byte short (so a small file is a cut connection, not a status), the audit log has a line `blocked`. A file that was cached before its digest was put on the list is refused with `403` from the cache.

Not built for the format `oci` (the settings refuse the words). An `allow` without `cooldown` is refused when the settings are read: it would do nothing.

## Checked

| What | How | Date |
|---|---|---|
| the rules and the verdicts: patterns, scoped names, the allow list lifting a cooldown and not a block, no time held back, the exact end of the cooldown | `internal/policy` | 2026-11-21 |
| `npm`: the young version out of the document, `latest` moved, the tags answer, the tarball `403` with the words and not fetched, the allow list, after the cooldown the version is served; the block list by name and version, a package whose versions are all blocked, a digest. `pypi`: the index and the files, a digest. `go`: the list, `@latest`, `.info`, `.mod`, `.zip`, after the cooldown. `maven`: the metadata and its checksum, the files. `raw`: a path pattern and a digest. Each guard broken on purpose to see its test fail | `internal/proxytest` | 2026-11-21 |
| the real `npm` 9.2.0, `pip` and `go` 1.26.11 against registry.npmjs.org, pypi.org and proxy.golang.org: a cooldown set between the ages of the two newest versions of `left-pad`, `six` and `github.com/google/uuid` (the client takes the older, the newer by name is `403`), and a block list naming the newest | `tools/test/policy-run.sh` | 2026-11-21 |

**Not run:** `mvn` with a block list against Maven Central; `yarn`, `pnpm`, `poetry`, `uv`; a cooldown through a group of several proxies; a cooldown of a version that was fetched before it was set, with the upstream down.
