# Verify a release

How-to. For releases made by the release workflow, **0.7.1 and later**: 0.6.0 and 0.7.0 have no files and no image. Nothing here needs a key of the project: the signatures are **keyless**, bound to the identity of the release workflow in this repository, and recorded in the public Sigstore log. You need `cosign` and `gh`.

The identity to expect: `https://github.com/zystem/godepot/.github/workflows/release.yml@refs/tags/vX.Y.Z`, issued by `https://token.actions.githubusercontent.com`. The commands below are those for 0.7.1, with what they printed.

## The binary

Download `godepot_0.7.1_linux_<arch>.tar.gz`, `checksums.txt` and `checksums.txt.sigstore.json` from the [release](https://github.com/zystem/godepot/releases/tag/v0.7.1), then check the files against the list:

```bash
sha256sum -c checksums.txt --ignore-missing
```

```
godepot_0.7.1_linux_amd64.tar.gz: OK
godepot_0.7.1_linux_arm64.tar.gz: OK
```

that the list was signed by the release workflow:

```bash
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/zystem/godepot/.github/workflows/release.yml@refs/tags/v0.7.1" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
```

```
Verified OK
```

and the build provenance (which repository and workflow made the file):

```bash
gh attestation verify godepot_0.7.1_linux_amd64.tar.gz -R zystem/godepot
```

The end of what it prints (the lines before it name the policy; a script, not a terminal, sees nothing and the exit code 0):

```
✓ Verification succeeded!

The following 1 attestation matched the policy criteria

- Attestation #1
  - Build repo:..... zystem/godepot
  - Build workflow:. .github/workflows/release.yml@refs/tags/v0.7.1
  - Signer repo:.... zystem/godepot
  - Signer workflow: .github/workflows/release.yml@refs/tags/v0.7.1
```

## The image

```bash
docker run --rm --read-only --cap-drop ALL ghcr.io/zystem/godepot:0.7.1 version
```

```
godepot 0.7.1
```

```bash
cosign verify ghcr.io/zystem/godepot:0.7.1 \
  --certificate-identity "https://github.com/zystem/godepot/.github/workflows/release.yml@refs/tags/v0.7.1" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The beginning of what it prints (the claims, as JSON, follow and are cut):

```

Verification for ghcr.io/zystem/godepot:0.7.1 --
The following checks were performed on each of these signatures:
  - The cosign claims were validated
  - Existence of the claims in the transparency log was verified offline
  - The code-signing certificate was verified using trusted certificate authority certificates
```

```bash
gh attestation verify oci://ghcr.io/zystem/godepot:0.7.1 -R zystem/godepot
```

It prints the same end as for the binary.

## The tag

The tags are annotated and **not signed**.

## The build is repeatable

The same source with the same Go version gives the same bytes, and CI checks it on every change (`reproducible` in `ci.yml`). `tools/release/package.sh X.Y.Z DIR`, run in a clean checkout of the tag with the Go version of `go.mod`, makes the archives of the release again: compare their SHA-256 with `checksums.txt`.

**0.7.1, arm64, does not compare.** The program in that archive is stamped as built from a changed tree (`go version -m` shows `v0.7.1+dirty`, `vcs.modified=true`): the workflow wrote the amd64 archive into the checkout before it built arm64. The source was that of the tag; the stamp makes the bytes differ. The amd64 archive compares. Fixed after 0.7.1.

## What was run

2026-11-28, on linux/amd64 with `cosign` 3.0.2, against 0.7.1: every command above; `cosign verify-blob` also for both archives and the bill of materials (each `Verified OK`), and with the identity of another tag (refused); the image pulled with no login; the amd64 archive made again, byte for byte the same. **Not run:** the arm64 program or image on an arm64 machine.
