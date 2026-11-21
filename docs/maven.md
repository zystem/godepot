# The format maven

Reference. The layout of a Maven 2 repository, as `mvn`, Gradle, sbt and Ivy read and write it, for hosted, proxy and group repositories of the format `maven`: artifacts, poms and signatures, `maven-metadata.xml`, the checksum files, snapshots, and what a proxy and a group do. How a proxy and a group behave in general (time to live, stale answers, pauses, claims) is in [proxy.md](proxy.md); the words of the settings file are in [settings-file.md](settings-file.md).

Like [gomod.md](gomod.md), [pypi.md](pypi.md) and [npm.md](npm.md) this format is built on `raw`: a file is a file of the repository at its Maven path, and the **artifact** (its group directories and its id, `com/acme/app`) is the name that claims and groups work with. So a group claims a whole group id with a prefix: `claims: ["com/acme/"]`.

## Use it

`~/.m2/settings.xml` (the token is the password; the user name is not looked at):

```xml
<settings>
  <servers><server><id>godepot</id><username>ci</username><password>TOKEN</password></server></servers>
  <mirrors><mirror><id>godepot</id><mirrorOf>*</mirrorOf><url>http://HOST/repository/REPOSITORY/</url></mirror></mirrors>
</settings>
```

```bash
mvn deploy        # with <distributionManagement><repository><id>godepot</id><url>http://HOST/repository/REPOSITORY/</url>
mvn dependency:get -Dartifact=com.acme:app:1.0
```

With the group as the mirror of `*`, plugins and every dependency come through it. `mvn` sends the credentials after the first `401`; a repository with `anonymous_read` needs none to read.

## The paths

| Path | What |
|---|---|
| `<group dirs>/<artifactId>/<version>/<artifactId>-<version>[-<classifier>].<ext>` | a file of a version: a jar, a pom, `-sources.jar`, `.module`, `.tar.gz`, a signature `.jar.asc` |
| `<group dirs>/<artifactId>/maven-metadata.xml` | the versions of an artifact |
| `<group dirs>/<artifactId>/<version>-SNAPSHOT/maven-metadata.xml` | the latest files of a snapshot |
| any of the above with `.sha1`, `.md5`, `.sha256` or `.sha512` | the checksum file: the hex digest, as text |
| a path that ends with `/` | the listing of raw ([raw.md](raw.md)) |

Each part of a path is `A-Z a-z 0-9 _ . + ~ @ -` and does not begin with a symbol. A path that is not of these shapes is not Maven's: a `PUT` to it is `400`, and a proxy hands it on as a plain file (`archetype-catalog.xml`, the metadata of a group of plugins).

## What the program makes

- **`maven-metadata.xml`** is **made at each request** from the files that are kept, and so is never out of step with them. The versions are in the order of Maven (`1.0-alpha-1` < `1.0-beta-2` < `1.0-rc1` < `1.0-SNAPSHOT` < `1.0` < `1.0-sp1` < `1.0.1` < `1.2` < `1.10`; numbers are compared as numbers, a missing part is `0`); `latest` is the highest, `release` the highest that is not a snapshot, `lastUpdated` the time of the newest file. The metadata of a snapshot has `timestamp`, `buildNumber` and, for each classifier and extension, the `value` (`2.0-20240102.110000-2`) of its newest file. It has an `ETag`. **A `maven-metadata.xml` that a client sends** (`mvn deploy` merges and sends one) **is answered `201` and not kept**, and so are its checksums.
- **The checksum files** are made of the file that is kept: SHA-1, MD5 and SHA-512 are worked out as the file comes in and are kept with it, SHA-256 is its digest. A **checksum file that a client sends** is checked against the file and not kept: it is `201` when it is the file's checksum (the file may be named after it, `<hex>  app-1.0.jar`), `400` when it is not, and `409` when the file is not there yet (a client sends a file first). SHA-1 and MD5 are what Maven names; they are names here, not a defence (the file's SHA-256 is its identity).

## Deploy

`PUT <path>` takes a file of a version. It needs the `write` action on the path. The refusals:

- the file name is not `<artifactId>-<version>[-<classifier>].<ext>` of its directories, or (a snapshot) has no `-<yyyyMMdd.HHmmss>-<n>` where `-SNAPSHOT` is (**unique snapshots only**: Maven 3 deploys so; a file named `app-1.0-SNAPSHOT.jar` is `400`);
- a **pom** (`<artifactId>-<version>.pom`) is not XML, or names another `artifactId` than the path, or another `groupId` or `version` (its own, or its parent's when it has none): so the pom a build reads is the pom of the place it is at. A pom that names no group or version and has no parent is refused (it cannot be checked);
- the body is over 4 GiB (`413`).

**A file is never overwritten** (R4): the same bytes again are `200` and change nothing; other bytes are `409` (the deploy of `mvn` fails). That holds for a release and for each timestamped file of a snapshot; a new snapshot build is a new file, and the metadata moves to it. There is no setting to overwrite (`overwrite_paths` is refused for this format). `DELETE <path>` deletes a file (`204`); the claim of the name (R3) stays. **Snapshot files pile up** until cleanup comes with the policies of step 7.

## A proxy

A proxy keeps what it fetched from an upstream that has the Maven layout (Maven Central, another godepot, Nexus, Artifactory).

| What | Rule |
|---|---|
| a file of a version (and a snapshot's timestamped file) | fetched once, when a client asks, **with its checksum checked** (P1): the upstream's `.sha512`, else `.sha256`, else `.sha1` is read first (and kept), and the bytes must have it; other bytes are not kept, the body is cut one byte short, the audit log has `upstream_changed` and `godepot_upstream_changed_total` counts it. **A file whose upstream has no checksum file is not served (`404`)**; the answer cannot tell that from a file that is not there |
| `maven-metadata.xml` and a path that is not a file of a version | trusted for `ttl` (default `5m`), then asked again; **served stale, with `Warning: 110`, when the upstream cannot answer** |
| the checksum of a `maven-metadata.xml` | **made of the copy that is held**, not fetched: the upstream's checksum may be of a newer metadata, and a client that reads both must see one pair |
| the checksum file of a file of a version | the upstream's, kept and served as it is |
| an artifact the upstream lacks | `404`, remembered for `negative_ttl` (default `10m`) |
| a write | `405` |

The checksum file is from the same upstream as the file, so it protects against a cut, a corrupt or a mixed-up download, not against an upstream that is itself wrong.

## A group

A group answers for an artifact from **the first member that has it**: a hosted member that holds a file below its directory, or a proxy whose cache holds it or whose upstream has its `maven-metadata.xml`. **The versions of one artifact are never mixed** from two members, and the metadata is the member's own (`merge` is not built). Claims (R3) are as in [proxy.md](proxy.md), by the artifact path: `claims: ["com/acme/"]` makes every artifact of `com.acme` ours, so the public one of the same name, with whatever version, is never asked. A write (`mvn deploy`) goes to `write_to`, authorized against the group. (A hosted member that has `com/acme/app/` below it for an artifact `app` also has the name for a group id `com.acme.app`: do not use both.)

## Checked

| What | How | Date |
|---|---|---|
| deploy of poms and jars with checksums, the same bytes again and other bytes, the checksum files (all four) and a wrong or early one, the metadata in the order of Maven and its checksum, an uploaded metadata not kept, a pom against its path (artifact, group, version, a parent), a path that is not Maven's, a signature, delete, anonymous write; snapshots (the latest files, the timestamp and build, a file with no timestamp refused, the pom against the snapshot version) | `internal/proxytest`, `internal/maven` | 2026-11-21 |
| a proxy: a file fetched once and checked, the strongest checksum used, the upstream's checksum file served, the checksum of the metadata made of the copy (the upstream's changed under it), the time to live, stale when down, a file that does not have its checksum, no checksum file, read-only; a group: `write_to`, a public name with a higher version not let in, a claimed prefix not asked, a name that stays claimed after delete; each guard broken on purpose to see its test fail | `internal/proxytest` | 2026-11-21 |
| the real `mvn` 3.8.7 (JDK 21): `deploy:deploy-file` of a release and a snapshot (twice) to a group, a release deployed again refused, `dependency:get` of both from the group; the plugin and `commons-io` 2.11.0 through a proxy of Maven Central (the group is the mirror of `*`), again with an empty local repository (no request upstream); a claimed name not found and not asked | `tools/test/maven-run.sh`, see [testing.md](testing.md) | 2026-11-21 |

**Not run:** Gradle, sbt, Ivy; an upstream other than Maven Central; a project built with `mvn deploy` (only `deploy:deploy-file`); `mvn release`; a non-unique snapshot (`-SNAPSHOT` in the file name, refused); the metadata of a group of plugins (`org/apache/maven/plugins/maven-metadata.xml`: a hosted repository does not make it, a proxy hands it on); an artifact with thousands of versions; a file near 4 GiB; checksum policy `fail` (`-C`) on the client.
