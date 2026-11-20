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
| `format` | **required** | `raw` ([raw.md](raw.md)) or `oci` ([oci.md](oci.md)). (Others come with later steps of the plan.) The name of an `oci` repository is the first part of the name of an image: `docker push HOST/<name>/<image>`. |
| `kind` | **required** | `hosted`. `proxy` and `group` are refused: they are not built yet. |
| `anonymous_read` | `false` | `true`: a request with no token may read (and list). Writing always needs a token. **Dangerous** on a repository that holds anything private. |
| `immutable_tags` | none | For the format `oci` only: patterns of the tags that never move once they name a manifest (`v*`, `release-*`); pushing another manifest under one is `409`. With none, every tag moves (`latest` does). A manifest is never overwritten: it is named by its digest. |
| `overwrite_paths` | none | For the format `raw` only: patterns of the paths that may be written again with other bytes. With none, a path once written is immutable: other bytes are `409` ([raw.md](raw.md)). |
| `quota_bytes` | `0` (no limit) | For the format `raw` only (the file is refused when an `oci` repository sets one). The most bytes the files of the repository may add up to (each file counted once however many repositories hold the same bytes). A write over it is `413` with the figures. |
| `quota_components` | `0` (no limit) | For `raw` only, as `quota_bytes`. The most files (for `raw`, a file is a component) the repository may hold. |

## `grants`

A grant gives an account actions on the paths of repositories. It is what the **account** may do; a token has a scope of its own and the request needs both ([access.md](access.md), [api.md](api.md)).

| Word | Default | Meaning |
|---|---|---|
| `account` | **required** | The name of an account. A grant for an account that does not exist is not an error (the account may be made later). |
| `repositories` | **required** | Patterns of repository names. |
| `paths` | every path | Patterns of the paths inside the repository. For `oci` the path is the image: `team/**` is every image under `team/` ([oci.md](oci.md)). |
| `actions` | **required** | `read`, `write`, `delete`, `promote`, `admin`. `admin` on a place is every action there. (`promote` has no meaning yet: nothing promotes.) |

A **pattern** has two wildcards: `*` is any run of characters but `/`, and `**` is any run of characters (`releases/**` is everything below `releases/`, and `releases/` itself; `*.tgz` is a file in the top directory). A pattern is at most 256 bytes with at most 4 runs of `*`: matching one costs a time that grows with the product of their reach, and patterns also come from users (the scope of a token).

An **admin account** (made with `new-account -admin`) may do every action on every repository, and needs no grant; its tokens still have their own scope.
