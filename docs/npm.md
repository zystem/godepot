# The format npm

Reference. The registry API of the `npm` command (and `yarn`, `pnpm`, which speak the same words) for hosted, proxy and group repositories of the format `npm`: the packument (the document of a package, full and abbreviated), tarballs, publish, dist-tags, deprecate and unpublish, scopes, and what a proxy and a group do. How a proxy and a group behave in general (time to live, stale answers, pauses, claims) is in [proxy.md](proxy.md); the words of the settings file are in [settings-file.md](settings-file.md).

Like [gomod.md](gomod.md) and [pypi.md](pypi.md) this format is built on `raw`: a tarball is a file of the repository, and the **package** (with its scope, in lower case) is the name that claims and groups work with.

## Use it

Make a token (the API, [api.md](api.md)) and put it in `.npmrc`, with the repository as the registry:

```ini
registry=http://HOST/repository/<repository>/
//HOST/repository/<repository>/:_authToken=<TOKEN>
```

```bash
npm publish                      # to a hosted repository, or to a group that has write_to
npm install left-pad             # from a hosted, proxy or group repository
npm dist-tag add pkg@1.1.0 latest
npm deprecate pkg@1.0.0 "use 1.1"
npm unpublish pkg@1.0.0 --force
```

`npm login` works for an account that has a password ([api.md](api.md)): `npm login --registry=http://HOST/repository/<repository>/ --auth-type=legacy` asks for the user name and the password, and writes a token (12 hours, the data actions of the account) to `.npmrc`. There is no sign up: an admin makes the account and sets the password. (Run with the real `npm` 9.2.0, typed at a terminal: [testing.md](testing.md).) A repository with `anonymous_read` needs no token to install.

## The requests

The paths follow the name of the repository. A scope may be sent as `@scope/name` or as `@scope%2fname` (npm sends the second); they are the same.

| Request | Answer |
|---|---|
| `GET <pkg>` | the packument. **Abbreviated** (`application/vnd.npm.install-v1+json`, what an install needs) when `Accept` has that type, else the **full** document (`application/json`); `Vary: Accept`. It has an `ETag`, and `If-None-Match` with it is `304`. `404` for a package with no versions |
| `GET <pkg>/-/<name>-<version>.tgz` | the tarball (with `Range`, `ETag`, as [raw.md](raw.md)). `<name>` is the part of the package name after the scope |
| `PUT <pkg>` | hosted (or a group with `write_to`): `npm publish` with a tarball in `_attachments`; without one, `npm deprecate` and tag changes. Below |
| `PUT <pkg>/-rev/<rev>` | the packument written back to a revision, as `npm unpublish` of one version does it: a version that is gone from it is deleted |
| `DELETE <pkg>/-/<name>-<version>.tgz/-rev/<rev>` | what `npm unpublish` sends after that: the tarball. `200` also when it is gone already |
| `DELETE <pkg>/-rev/<rev>` | the whole package (all versions). `409` with a revision that is not the revision there is |
| `GET -/package/<pkg>/dist-tags` | the tags, `{"latest": "1.0.0", ...}` |
| `PUT -/package/<pkg>/dist-tags/<tag>` | the body is the version, a JSON string (`npm dist-tag add`). `404` for a version that is not there |
| `DELETE -/package/<pkg>/dist-tags/<tag>` | `npm dist-tag rm`. `latest` is not removed (`400`): move it |
| `GET -/ping` | `200 {}` (`npm ping`) |
| `GET -/whoami` | `{"username": "<account>"}`; `401` with no token |
| `PUT -/user/org.couchdb.user:<name>` `{"name", "password"}` | `npm login`: `201` and `{"ok": true, "token": "gd_..."}`; `401` for a login that is not good, `429` for a locked account; `400` when the name is not the address's |
| the rest of `-/...` | `404`: **audit and search are not built**. `npm install` goes on (set `audit=false` to not ask) |

Every error is `{"error": "<words>"}`. A write to a proxy is `405`.

**The address of a tarball** in a packument is whole (`http://HOST/repository/<repository>/<pkg>/-/<file>`): npm needs it so. The scheme is `https` when the request came over TLS or has `X-Forwarded-Proto: https`, and the host is the `Host` of the request. The address of the **group** is used when the request was sent to a group, so a client stays on it. A lock file (`package-lock.json`) keeps these addresses in `resolved`.

**A grant's `paths`** match the package name (`@corp/**`), and for a tarball, `<pkg>/-/<file>`.

## What is kept

A hosted repository keeps, for a version, two files: the tarball at `<pkg>/-/<name>-<version>.tgz` and the manifest at `<pkg>/-/<version>.json` (the `package.json` as published, with `dist` made by godepot: `shasum` and `integrity`, and no `readme` and no key that begins with `_`). Neither is an address of a client. **Dist-tags and deprecation messages are properties** of the manifest's component (`tag.<name>`, `deprecated`); a tag names one version (when a write was cut short and two have it, the one written last).

The packument is **made** at each request from what is kept: the tarball address, `dist-tags`, the times (`time`: when each version was written, `created`, `modified`), a `_rev` (a hash of the versions, their digests, deprecations and tags: it changes with every change, so a write to a revision that was read before another write is `409`). The abbreviated form has `name`, `modified`, `dist-tags` and, for each version, only the keys an install reads (`dependencies`, `bin`, `engines`, `dist`, `deprecated`, `hasInstallScript`, ...).

## Publish

`npm publish` sends a `PUT <pkg>` with JSON: `name`, `dist-tags`, `versions` (the manifest of the new version) and `_attachments` (the tarball in base64). The whole request is **read into memory** (the tarball is inside the JSON): a publish is at most **128 MiB** (`413`). The refusals:

- the name in the body is not the package of the address; the name is not one that may be published (lower case, `a-z 0-9 . _ ~ -`, a scope as `@scope/name`, at most 214 bytes); the version is not a semantic version (`1.0.0`, `1.2.3-rc.1`);
- more than one attachment (a provenance bundle is not taken), or one that is not `<name>-<version>.tgz` (npm names it with the scope: `@corp/lib-1.0.0.tgz`);
- not one tag (`npm publish --tag`, default `latest`), or a tag that is a version, or one for another version;
- the tarball is not a gzip of a tar, has more than 100 000 entries or more than 2 GiB unpacked, has a name with `..`, `\` or a leading `/`, a link that leaves the directory, or **no `package/package.json`**; or that `package.json` names another package or version than the publish, or the manifest does (so what a client unpacks is what the packument says);
- `dist.shasum` or `dist.integrity` in the manifest, when sent, is not that of the bytes.

**A version is never overwritten** (R4): the same bytes again are `200` and change nothing; other bytes are `409` (npm says `EPUBLISHCONFLICT`). The claim (R3) of the name is written in the same transaction that publishes the first file, and stays after the package is deleted. `201` on success. The tag is given to this version and taken off the others.

`npm deprecate` writes the packument back with `deprecated` on versions: it changes the messages (at most 4096 bytes). A new version without a tarball is `400`; versions that are gone are `400` unless the write is to a revision (`npm unpublish`).

## A proxy

A proxy keeps what it fetched from an upstream of the registry API (registry.npmjs.org, another godepot, Verdaccio): the packument of a package is cached as a file of the proxy (`-/index/<pkg>.json`, not an address of a client) and tarballs are fetched from the addresses the packument names.

| What | Rule |
|---|---|
| the packument | asked in the **abbreviated** form and trusted for `ttl` (default `5m`), then asked again; **served stale, with `Warning: 110`, when the upstream cannot answer**. The client gets the document **made from** it, with each tarball address made ours; versions whose own manifest does not name this package and this version are left out |
| a tarball | fetched once, when a client asks, from the address the packument names (the upstream's own host, or one in `file_hosts`), **with its hash checked** (P1): `dist.integrity` (sha512, else sha256) when the packument has one, else `dist.shasum` (sha1). Other bytes are not kept, the body is cut one byte short, the audit log has `upstream_changed`, and `godepot_upstream_changed_total` counts it. **A tarball whose packument names no hash is refused** |
| a package the upstream lacks | `404`, remembered for `negative_ttl` (default `10m`) |
| a write | `405` |

`npm view` and the like through a proxy show the **abbreviated** document: no `readme`, no `time`, and only the keys above. (The full one is not copied: it is much larger.) Names with capitals (old packages) are read; they cannot be published.

## A group

A group answers for a package from **the first member that has it**: a hosted member that holds a version of it, or a proxy whose cache holds the packument or whose upstream has the package. The versions of one package are never mixed from two members (`merge` is not built). **Claims** (R3) work as in [proxy.md](proxy.md), with the package name **in lower case**, so a name with capitals does not get round a claim: `claims: ["@corp/", "corp-"]`. A write (`npm publish`, `dist-tag`, `unpublish`) goes to `write_to`, authorized against the group; the address in the packument is the group's.

## Checked

| What | How | Date |
|---|---|---|
| publish with a scope, the abbreviated and full packument (encoded and plain scope), `ETag`/`304`, the tarball, the same bytes again and other bytes, tags added, moved, removed, deprecate with a revision and a stale revision, a new version with no tarball, unpublish of a version and of the package; every refusal above (name, version, tag, tarball, hash, manifest, anonymous) | `internal/proxytest`, `internal/npm` | 2026-11-20 |
| a proxy: the abbreviated packument made ours, the tarball fetched once and its hash checked (sha512, and sha1 when that is all there is), a changed tarball cut and not kept, no hash named, a packument that names another package, the time to live, stale when down, read-only; a group: `write_to`, our name not asked of the public one, a claimed scope, capitals, a name that stays claimed after delete | `internal/proxytest`; each guard broken on purpose to see its test fail | 2026-11-20 |
| the real `npm` 9.2.0 (node 18.19.1): `publish` to a group, `install`, `dist-tag`, `deprecate`, a second publish refused, `unpublish`; `left-pad` and a scoped package through a proxy of registry.npmjs.org, again with an empty npm cache (no request upstream); a claimed name and scope not found and not asked | `tools/test/npm-run.sh`, see [testing.md](testing.md) | 2026-11-20 |

**Not run:** `yarn` and `pnpm` (they use the same API); an upstream other than registry.npmjs.org; a package with hundreds of versions (the packument of a hosted repository reads each manifest from the store: slow for very large ones); a tarball of hundreds of MiB through a publish; `npm install` with TLS and `X-Forwarded-Proto` behind a reverse proxy; `npm publish --provenance`, `npm access`, `npm owner`, `npm team`, `npm token` and `npm search`, which are not served; `npm audit`, which is answered `404`.
