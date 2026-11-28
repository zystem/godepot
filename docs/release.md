# Cut a release

How-to. A release is a tag; the workflow `.github/workflows/release.yml` does the rest.

1. In a pull request: update [supported-versions.md](supported-versions.md); move what is under `## [Unreleased]` in [CHANGELOG.md](../CHANGELOG.md) to `## [X.Y.Z] - <date>` (that text is the text of the release; the workflow stops before publishing anything when the section is missing), and bring the README's status and [status.md](status.md) up to date. Merge it and wait for `ci` on `main` to be green.
2. Tag that commit and push the tag:

   ```bash
   git tag -a vX.Y.Z -m "godepot X.Y.Z" && git push origin vX.Y.Z
   ```

   It prints the new tag that was pushed.
3. The workflow runs `ci` on the tagged commit, then publishes:
   * the image `ghcr.io/zystem/godepot:X.Y.Z`, linux/amd64 and linux/arm64, with provenance and an SBOM; also `X.Y` for a version with no suffix, and `latest` from 1.0 on;
   * the GitHub release with `godepot_X.Y.Z_linux_<arch>.tar.gz`, `checksums.txt` and `godepot.spdx.json`, each with a `.sigstore.json` (keyless signature);
   * a build provenance attestation for the image and the archives.

   A version before 1.0, or with a suffix (`vX.Y.Z-rc.1`), is marked as a pre-release.
4. Check what was published, as a user would ([verifying-releases.md](verifying-releases.md)).

The image `ghcr.io/zystem/godepot` is public (it can be pulled with no login; checked after 0.7.1). In a copy of this repository, look at the visibility of the new package: the workflow does not set it.

A tag is not moved: a release that is wrong is replaced by the next version.

## What was run

2026-11-28: the workflow, for `v0.7.1`, its first run, passed; what it published was checked as [verifying-releases.md](verifying-releases.md) says. It showed one fault (the arm64 program was stamped as built from a changed tree), fixed in `tools/release/package.sh`: the fixed script, run in a clean copy with the archives written inside it, stamped both programs as unchanged. **The fixed script has not run in the workflow yet.** **Not run:** a tag with a suffix, a version from 1.0 on (`latest`).
