# Verify a release

How-to. For releases made by the release workflow, that is **after 0.7.0**: 0.6.0 and 0.7.0 have no files and no image. Nothing here needs a key of the project: the signatures are **keyless**, bound to the identity of the release workflow in this repository, and recorded in the public Sigstore log.

The identity to expect: `https://github.com/zystem/godepot/.github/workflows/release.yml@refs/tags/vX.Y.Z`, issued by `https://token.actions.githubusercontent.com`.

**These commands have not been run**: there is no signed release yet. What they print is added after the first one.

## The binary

Download `godepot_X.Y.Z_linux_<arch>.tar.gz`, `checksums.txt` and `checksums.txt.sigstore.json` from the [releases](https://github.com/zystem/godepot/releases), then:

```bash
sha256sum -c checksums.txt --ignore-missing
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/zystem/godepot/.github/workflows/release.yml@refs/tags/vX.Y.Z" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
gh attestation verify godepot_X.Y.Z_linux_amd64.tar.gz -R zystem/godepot
```

The first line checks the files against the list, the second that the list was signed by the workflow, the third the build provenance (which commit, which workflow).

## The image

```bash
cosign verify ghcr.io/zystem/godepot:X.Y.Z \
  --certificate-identity "https://github.com/zystem/godepot/.github/workflows/release.yml@refs/tags/vX.Y.Z" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/zystem/godepot:X.Y.Z -R zystem/godepot
```

## The tag

The tags are annotated and **not signed**.

## The build is repeatable

The same source with the same Go version gives the same bytes, and CI checks it on every change (`reproducible` in `ci.yml`). `tools/release/package.sh X.Y.Z DIR`, run in a checkout of the tag with the Go version of `go.mod`, makes the archives of the release again: compare their SHA-256 with `checksums.txt`.
