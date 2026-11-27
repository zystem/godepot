# Cut a release

How-to. A release is a tag; the workflow `.github/workflows/release.yml` does the rest. **The workflow has not run yet** (see [What was run](#what-was-run)).

1. In a pull request: move what is under `## [Unreleased]` in [CHANGELOG.md](../CHANGELOG.md) to `## [X.Y.Z] - <date>` (that text is the text of the release; the workflow stops before publishing anything when the section is missing), and bring the README's status and [status.md](status.md) up to date. Merge it and wait for `ci` on `main` to be green.
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

**The first time only:** a package that GitHub makes is **private** until its owner makes it public (Packages, the package, Package settings, Change visibility). The workflow cannot do it.

A tag is not moved: a release that is wrong is replaced by the next version.

## What was run

2026-11-28, by hand on one machine: `tools/release/package.sh` twice gave the same SHA-256 for both archives, and the amd64 program printed its version; `tools/release/notes.sh` printed the section of 0.7.0 and failed for a version with no section; the image was built for both platforms with provenance and SBOM, not pushed; `actionlint` found nothing in the workflows. **Not run:** the workflow itself (the publishing, the signatures, the attestations): the first tag after it is merged is its first run.
