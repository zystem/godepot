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
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w -X main.version=$ver" -o "$work/godepot" ./cmd/godepot
  install -m 0644 LICENSE "$work/LICENSE"
  chmod 0755 "$work/godepot"
  tar -C "$work" --sort=name --mtime="@$at" --owner=0 --group=0 --numeric-owner -cf - LICENSE godepot | gzip -n >"$out/godepot_${ver}_linux_${arch}.tar.gz"
done
(cd "$out" && sha256sum "godepot_${ver}_linux_amd64.tar.gz" "godepot_${ver}_linux_arm64.tar.gz" >checksums.txt)
