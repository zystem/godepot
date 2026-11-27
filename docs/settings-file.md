# The settings file

Reference. The file that `GODEPOT_CONFIG_FILE` names ([configuration.md](configuration.md)): the repositories, and which account may do what in them. JSON, because the program has no other format in its standard library; every word that is not below is an error. `godepot validate` checks it and names every problem at once; `serve` reads it at start and again on `SIGHUP`, and keeps the settings in use when the new file is wrong.

Accounts and tokens are **not** in the file: they are made while the program runs and live in the database ([access.md](access.md), [api.md](api.md)). The file says what an account may do; the database says who the account is.

```json
{
  "repositories": [
    {"name": "files", "format": "raw", "kind": "hosted", "overwrite_paths": ["latest/**"], "quota_bytes": 10737418240},
    {"name": "public", "format": "raw", "kind": "hosted", "anonymous_read": true}
  ],
  "grants": [
    {"account": "ci-build", "repositories": ["files"], "paths": ["releases/**"], "actions": ["read", "write"]},
    {"account": "reader", "repositories": ["*"], "actions": ["read"]}
  ]
}
```

## `repositories`

| Word | Default | Meaning |
|---|---|---|
| `name` | **required** | 1 to 63 of `a-z 0-9 . _ -`, beginning with a letter or a digit. It is in the address: `/repository/<name>/...`. Unique. |
| `format` | **required** | `raw` ([raw.md](raw.md)), `oci` ([oci.md](oci.md)) or `go` ([gomod.md](gomod.md)) or `pypi` ([pypi.md](pypi.md)) or `npm` ([npm.md](npm.md)) or `maven` ([maven.md](maven.md)). (Others come with later steps of the plan.) The name of an `oci` repository is the first part of the name of an image: `docker push HOST/<name>/<image>`. |
| `kind` | **required** | `hosted` (written by you), `proxy` (a copy, made on demand, of the `upstream` below) or `group` (the `members` below, read as one). [proxy.md](proxy.md) says what each does. |
| `upstream` | **required for a proxy**, refused otherwise | The registry a proxy copies from: the object below. |
| `members` | **required for a group**, refused otherwise | The names of the repositories a group answers from, in the order in which they answer. All must be of the group's format; a member may be a group; a group may not hold itself, directly or through others. |
| `write_to` | none | For a group: the hosted member that a write to the group (a `PUT`, a push) goes to. With none, a write to the group is `405`. Must be a hosted member. |
| `claims` | none | For a group: prefixes of names (a raw path, or an image name without the repository) that belong to the hosted members before anything is published under them. A name under a claim that no hosted member has is `404` and is never asked of a proxy. No wildcards: `corp/` claims everything that begins with `corp/`. A name that a hosted member **has published** is claimed without being listed. |
| `merge` | `false` | Would make a group merge the versions of one name from all its members. **Not built**: `true` is refused. A group stops at the first member that has the name. |
| `anonymous_read` | `false` | `true`: a request with no token may read (and list). Writing always needs a token. **Dangerous** on a repository that holds anything private. |
| `immutable_tags` | none | For the format `oci` only: patterns of the tags that never move once they name a manifest (`v*`, `release-*`); pushing another manifest under one is `409`. With none, every tag moves (`latest` does). A manifest is never overwritten: it is named by its digest. |
| `overwrite_paths` | none | For the format `raw` only (refused for `go`, `pypi`, `npm` and `maven`: a published version or file is never overwritten): patterns of the paths that may be written again with other bytes. With none, a path once written is immutable: other bytes are `409` ([raw.md](raw.md)). |
| `quota_bytes` | `0` (no limit) | For the format `raw` only (the file is refused when an `oci` repository sets one). The most bytes the files of the repository may add up to (each file counted once however many repositories hold the same bytes). A write over it is `413` with the figures. |
| `quota_components` | `0` (no limit) | For `raw` only, as `quota_bytes`. The most files (for `raw`, a file is a component) the repository may hold. |
| `cleanup` | none | A policy that removes what is old, for a hosted or a proxy repository of a format other than `oci` ([cleanup.md](cleanup.md)). An object with the words below. It runs every `GODEPOT_CLEANUP_SECONDS` and writes a report; it removes only with `enforce`. |
| `enforce` (in `cleanup`) | `false` | Remove what the report names (to the trash). **With `false` the policy only reports.** A policy that has `enforce` and no `remove_older_days` is refused. |
| `remove_older_days` (in `cleanup`) | not set: nothing is removed | Remove the versions whose newest file was written more than this many days ago and that no keep rule holds. `0` is every version that is not kept. |
| `keep_newest` (in `cleanup`) | `0` (this rule keeps none) | Keep the newest N versions of each name, by when they were written. For the format `raw` every file is a name of its own, so any N of 1 or more keeps all. |
| `keep_read_days` (in `cleanup`) | `0` (this rule keeps none) | Keep what was read in the last D days. The time of a read is kept to the day. |
| `keep` (in `cleanup`) | none | Patterns of names that are kept (`org/keep/**`; for `raw`, the paths). |
| `keep_props` (in `cleanup`) | none | Keep what has all of these properties: an object of key and value (`{"pin": "yes"}`). |

### `upstream` (of a proxy)

| Word | Default | Meaning |
|---|---|---|
| `url` | **required** | The address of the registry: its origin, and a path when it has one (`https://registry-1.docker.io`, `https://example.com/files`). `http` or `https`; no user name, query or fragment in it. Only you set it: it is never taken from a request (P9). |
| `username` | none | The user name for basic authentication; needs `password_env`. |
| `password_env` | none | The **name of an environment variable** that holds the password. The password is never in the file. godepot refuses to use the repository (`502`) when the variable is not set, instead of asking with no credentials. |
| `token_env` | none | The name of an environment variable that holds a bearer token, sent as `Authorization: Bearer`. Not with `password_env`. |
| `token_hosts` | none | Hosts, besides the upstream's own, that may be sent the credentials when the upstream answers `401` and names them as its token service (Docker Hub: `auth.docker.io`). With none, a token service on another host is asked with no credentials: an anonymous token, which is what Docker Hub gives for public images. Credentials are never sent after a redirect to another host. |
| `file_hosts` | none | Hosts, besides the upstream's own, that an index of the upstream may name for its files (PyPI: `files.pythonhosted.org`; npm: a host that the packument names for tarballs, when it is not the upstream's own). A file on any other host is not fetched. They are never sent credentials. |
| `cooldown` | none | For a proxy of the format `npm`, `pypi` or `go`. A version that the upstream published less than this long ago (a string of Go: `168h` is 7 days) is **left out of the indexes this repository serves and refused by name** (`403`, with the time when it will be served) ([policy.md](policy.md)). Where the upstream gives no time for a version it is held back too. Not for `oci`, `maven`, `raw`. |
| `allow` | none | Entries (`name`, `name@version`, patterns) that the cooldown does not hold. Needs `cooldown`. It lifts the cooldown and nothing else: a `block` still wins. |
| `block` | none | Entries that are never served, in the indexes or by name: `name`, `name@version` (patterns: `*` is any run of characters but `/`, `**` any run) and `sha256:<64 hex digits>` (the bytes with that digest are never kept or served). For a proxy of every format but `oci`. |
| `allow_private` | `false` | `true`: fetch from loopback, private and link-local addresses. With `false` such an address is refused **after the name is resolved**, for the upstream, for every redirect and for the token service. Set it for an upstream inside your network. |
| `max_per_second` | `0` (no limit) | The most requests a second to this upstream. |
| `max_concurrent` | `16` | The most requests to this upstream at once. |
| `ttl` | `5m` for `oci`, `go`, `pypi`, `npm` and `maven`, none for `raw` | How long what can change is trusted before the upstream is asked again: an OCI tag, the list and the latest of a Go module, the index of a Python project, the packument of an npm package, the `maven-metadata.xml` of Maven; for `raw`, a file (**with none, a raw file is fetched once and never asked again**). A string of Go: `90s`, `5m`, `24h`. What cannot change (a blob, a manifest by digest) is never asked again. |
| `negative_ttl` | `10m` | How long a "not found" is remembered, per name. `0s`: not at all. An error of the upstream is never remembered as "not found". |
| `immutable_paths` | none | For `raw`: patterns of the files that cannot change upstream. When the upstream gives other bytes for one after `ttl`, the cached bytes stay, the audit log has a line, and `godepot_upstream_changed_total` counts it. |

A change of an upstream's words is taken at `SIGHUP` with the rest of the file.

## `issuers`

Who is trusted to sign the tokens of CI jobs ([ci-jobs.md](ci-jobs.md)). With none, `POST /api/v1/oidc/token` accepts nothing. Read again on `SIGHUP`.

| Word | Default | Meaning |
|---|---|---|
| `name` | **required** | 1 to 63 of `a-z 0-9 . _ -`. A trust names it. Unique. |
| `url` | **required** | The `iss` of the issuer's tokens, compared to the character (`https://token.actions.githubusercontent.com` has no `/` at the end). `https`; `http` only with `allow_private`. Its discovery document is at `<url>/.well-known/openid-configuration`. Unique. |
| `audience` | **required** | The `aud` a token must have. Use the address of this godepot, so that a token made for another service is not taken. |
| `jwks_url` | from the discovery document | The address of the issuer's keys, for an issuer whose discovery document cannot be used. |
| `allow_private` | `false` | **Dangerous with an address you do not control.** `true`: the keys may be read from a loopback, private or link-local address, and over `http`. |

## `trusts`

Which job may become which robot. A trust is tried in the order of the file; the first whose issuer and `claims` fit is used (the request may name the `account` when several fit).

| Word | Default | Meaning |
|---|---|---|
| `issuer` | **required** | The `name` of an issuer. |
| `account` | **required** | A **robot** account that is not an admin. It must exist and be enabled when the job asks, not when the file is read. Its grants limit the token. |
| `claims` | **required** | **The lock: read [ci-jobs.md](ci-jobs.md).** An object of claim name and pattern (the same two wildcards as below); **every** one must fit. At least one of `sub`, `repository`, `repository_id`, `repository_owner`, `repository_owner_id`, `project_path`, `project_id`, `namespace_path`, `namespace_id` must be there with a value that does not begin with `*`. A claim that the token lacks, or that is a list or an object, does not fit. |
| `repositories` | `["*"]` | Patterns of the repositories of the token. |
| `paths` | every path | Patterns of the paths of the token. |
| `actions` | `["read", "write"]` | `read`, `write`, `delete`, `promote`. `admin` is refused. |

## `ldap`

The directory people log in with ([ldap.md](ldap.md)). With none, only local accounts have passwords. Read again on `SIGHUP`; the environment variable is read at each use.

| Word | Default | Meaning |
|---|---|---|
| `url` | **required** | `ldaps://host:636`, or `ldap://host:389` with `start_tls`. No path, no user name. |
| `start_tls` | `false` | Encrypt an `ldap` connection with StartTLS before anything is sent. **Required for `ldap`**, refused for `ldaps`. |
| `ca_file` | the system's | A PEM file with certificates to trust: an authority's, or a self-signed server's own. They may also be given through the API (`PUT /api/v1/ldap/ca`, [ldap.md](ldap.md)), with no file. With either, **only** those are trusted. |
| `server_name` | the host of `url` | A name the server's certificate must have. Not needed when the server shows exactly a certificate you gave (`ca_file` or the API): then the name is checked only if this is set ([ldap.md](ldap.md)). |
| `bind_dn` | **required** | The service account that searches. It needs to read only. |
| `password_env` | **required** | The **name of an environment variable** with its password. |
| `user_base` | **required** | Where people are searched (the whole subtree). |
| `user_filter` | **required** | An LDAP filter with `{user}` (the name that logs in, escaped). It must find one entry, and **should not find disabled people**. |
| `group_base` | none: no groups | Where groups are searched. Grants for `group` need it. |
| `group_filter` | `(member={dn})` | The groups of a person; `{dn}` is the person's entry, `{user}` the name. One of them must be there. |
| `group_name` | `cn` | The attribute of a group that a grant names. |
| `refresh` | `5m` | How long what the directory said of a person is trusted before it is asked again. |

## `grants`

A grant gives an account actions on the paths of repositories. A grant on a **group** is what a client of the group needs: it reads everything the group answers with, whichever member has it, and writes to `write_to`; the grants of the members are not asked. It is what the **account** may do; a token has a scope of its own and the request needs both ([access.md](access.md), [api.md](api.md)).

| Word | Default | Meaning |
|---|---|---|
| `account` | one of `account`, `group` | The name of an account. A grant for an account that does not exist is not an error (the account may be made later). |
| `group` | one of `account`, `group` | A group of the directory (`ldap`), compared without regard to capitals: the people in it have the grant. Local accounts have no groups. |
| `repositories` | **required** | Patterns of repository names. |
| `paths` | every path | Patterns of the paths inside the repository. For `oci` the path is the image: `team/**` is every image under `team/` ([oci.md](oci.md)). |
| `actions` | **required** | `read`, `write`, `delete`, `promote`, `admin`. `admin` on a place is every action there. (`promote` has no meaning yet: nothing promotes.) |

A **pattern** has two wildcards: `*` is any run of characters but `/`, and `**` is any run of characters (`releases/**` is everything below `releases/`, and `releases/` itself; `*.tgz` is a file in the top directory). A pattern is at most 256 bytes with at most 4 runs of `*`: matching one costs a time that grows with the product of their reach, and patterns also come from users (the scope of a token).

An **admin account** (made with `new-account -admin`) may do every action on every repository, and needs no grant; its tokens still have their own scope.
