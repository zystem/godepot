# Cleanup and the trash

Reference. What a cleanup policy removes, how it decides, what it reports, and where what is removed goes. The words of the settings file are in [settings-file.md](settings-file.md), the calls in [api.md](api.md), the two settings of the program in [configuration.md](configuration.md). The requirements are C1 to C4 of [spec.md](spec.md). Tested by `internal/proxytest` (`cleanup_test.go`) with a real rqlite and blobgate; [testing.md](testing.md) says what was run.

Built for the formats `raw`, `go`, `pypi`, `npm` and `maven`, hosted and proxy repositories. **Not built:** the format `oci` (its tags, manifests and links are other tables: a policy for it is refused), `last read` of an OCI blob, and a policy on a group (clean the members).

## A version

A policy works on **versions**: the files of one name and one version go together. What a version is comes from the format:

| Format | Name | Version | Its files |
|---|---|---|---|
| `raw` | the path | none | the file |
| `go` | the module | the version | `.info`, `.mod`, `.zip` |
| `pypi` | the project | the version of the file name | the distributions and their `.metadata` |
| `npm` | the package | the version | the manifest and the tarball |
| `maven` | `group/dirs/artifactId` | the version directory | every file of it |

A file that is not part of a version (a list of versions, the cached index of a proxy, a `maven-metadata.xml`) is never removed by a policy.

## The rules

A policy is the object `cleanup` of a repository. **Removing** has one rule, `remove_older_days`: a version whose newest file was written more than that many days ago is removed (`0`: every version). **Keeping** has four, and a keep rule always wins:

- `keep_newest`: the newest N versions of a name, by when they were written (a tie is broken by the version, the higher first);
- `keep_read_days`: what was read in the last D days;
- `keep`: names that match a pattern;
- `keep_props`: what has all the given properties.

With `remove_older_days` not set the policy removes **nothing**, and its report says so (`note`). `enforce: true` without it is refused when the settings are read.

**The time of a read** (C4) is kept to the day, per component. A read only notes the component in memory; each replica writes what it noted once a minute and when it ends, in one statement for each 400 components. A read never waits for the database. A read of a proxy's cache counts like any other; a version that was never read has no read time, so `keep_read_days` does not hold it (`remove_older_days` and the age do).

## The report

`GET /api/v1/repositories/<name>/cleanup` makes the report now and changes nothing. The same report is made, and kept as "the last", every time the policy runs (every `GODEPOT_CLEANUP_SECONDS` on the replica that leads, or `POST` on the same address):

```json
{"repository": "mvn", "enforced": false,
 "considered": 5, "kept": 2,
 "remove": [{"name": "com/acme/app", "version": "1.0", "assets": 2, "bytes": 4151, "age_days": 20}, ...],
 "bytes": 9302, "freed": 4302}
```

`bytes` is the sum of the sizes of the files that go. `freed` is what is **really** freed: a blob that anything else also names (another version, another repository, the trash) is not counted. It is freed when the item is emptied from the trash. **A dry report is what enforcing then removes** (the same list, `bytes` and `freed`) as long as nothing is written between the two; the test makes the report, enforces, and compares the list and then the bytes that are gone from the store.

## Enforcing, and the trash

With `enforce: true` a run moves each version of the report to the **trash** (C3), one transaction for each, and the report says `removed` for each (or `error`). The audit log has a line `remove` for each and one `cleanup` for the run. A person's `DELETE` of a file of a format built on raw goes to the trash too (`reason: delete`); a delete of an OCI manifest or tag does not yet.

The trash holds the rows of the removed files and keeps their blobs held: **nothing is freed**, and the item can come back **whole, with the same ids** (`POST /api/v1/trash/<id>/restore`): if a path of it has been written meanwhile, the restore is `409` and the item stays. An item is emptied after `GODEPOT_TRASH_DAYS` (default 7) by the replica that leads, or at once with `DELETE /api/v1/trash/<id>`; its bytes are then collected after the grace time of the blob layer ([blob-layer.md](blob-layer.md)). With `GODEPOT_TRASH_DAYS=0` there is no trash: a removal frees at once and cannot be undone.

What a client sees: a removed file is `404`, a removed version is gone from `maven-metadata.xml`, the packument and the index at once. A **name that was published stays claimed** in the groups that hold the repository, so cleanup does not give a name to an upstream (R3).

## Checked

| What | How | Date |
|---|---|---|
| a dry report names the versions that `remove_older_days` and `keep_newest` say; a run with `enforce` off removes nothing; the same run with `enforce` removes the same list with the same `bytes` and `freed`; the metadata no longer lists them; the trash frees nothing while the items are in it; after they are emptied, the bytes gone from the store are the bytes the report said (a blob that a kept version shares is not counted) | `internal/proxytest` | 2026-11-21 |
| the keep rules (read in the last days, a name pattern, a property), no `remove_older_days` removes nothing and says why | `internal/proxytest` | 2026-11-21 |
| a delete goes to the trash, restore with the same ETag, `409` when the path was written since, `404` when it is gone, the sweep after the days, the bytes freed when it is emptied, admin only | `internal/proxytest` | 2026-11-21 |

**Not run:** a policy through the background loop of a running program (the tests call the same code); two replicas flushing reads and running policies at once (only the one that leads runs policies; each flushes its own reads); a repository of a million versions; a policy on a proxy repository.
