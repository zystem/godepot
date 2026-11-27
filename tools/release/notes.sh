#!/usr/bin/env bash
# The text of a release: notes.sh VERSION prints the section "## [VERSION]" of CHANGELOG.md, its links to files made whole for the tag. It fails when there is no such section: a release says what is in it.
set -euo pipefail
ver="${1:?notes.sh VERSION}"
cd "$(dirname "$0")/../.."
text="$(awk -v h="## [$ver]" 'index($0, h) == 1 {on = 1; next} /^## \[/ {on = 0} on' CHANGELOG.md)"
[ -n "${text//[$'\n ']/}" ] || { echo "notes.sh: CHANGELOG.md has no section \"## [$ver]\"" >&2; exit 1; }
base="https://github.com/${GITHUB_REPOSITORY:-zystem/godepot}/blob/v$ver"
printf '%s\n' "$text" | sed -E "s#\]\((docs/[^)]+|README\.md[^)]*)\)#]($base/\1)#g"
