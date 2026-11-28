#!/usr/bin/env bash
# The files of a release: package.sh VERSION DIR puts godepot_VERSION_linux_{amd64,arm64}.tar.gz (the program and LICENSE) and checksums.txt in DIR. The same commit and Go give the same bytes:
# the times in the archives are that of the commit.
set -euo pipefail
ver="${1:?package.sh VERSION DIR}" out="${2:?package.sh VERSION DIR}"
cd "$(dirname "$0")/../.."
at="$(git log -1 --format=%ct)"
mkdir -p "$out"
work="$(mktemp -d)"
trap 'rm -rf "${work:?}"' EXIT
# Both programs are built before anything is written to DIR: when DIR is inside the checkout, a file there makes Go stamp the next program as built from a changed tree ("+dirty"), and it is then not
# the program that the tag builds. 0.7.1 was released so (its arm64).
for arch in amd64 arm64; do
  mkdir "$work/$arch"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w -X main.version=$ver" -o "$work/$arch/godepot" ./cmd/godepot
  chmod 0755 "$work/$arch/godepot"
  install -m 0644 LICENSE "$work/$arch/LICENSE"
done
for arch in amd64 arm64; do
  tar -C "$work/$arch" --sort=name --mtime="@$at" --owner=0 --group=0 --numeric-owner -cf - LICENSE godepot | gzip -n >"$out/godepot_${ver}_linux_${arch}.tar.gz"
done
(cd "$out" && sha256sum "godepot_${ver}_linux_amd64.tar.gz" "godepot_${ver}_linux_arm64.tar.gz" >checksums.txt)
