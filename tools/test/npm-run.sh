#!/usr/bin/env bash
# The proof of the format npm (step 6 of the plan), run against the program with the real client: `npm publish` sends a package to a group (it goes to the group's write_to), `npm install` takes it
# from the group, `npm dist-tag`, `npm deprecate` and `npm unpublish` change it; public packages are installed through a proxy of registry.npmjs.org, the second time (with an empty npm cache)
# sending nothing upstream; a claimed name is not asked of npmjs. It needs the stand of blobgate-stand.sh, npm and node, and the network:
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/npm-run.sh
# It starts the program on port 18690 (metrics 18692), prints one line for each, and exits 1 when anything that ran failed; what needs the network says skipped.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${GODEPOT_BLOBGATE_URL:?run the stand first}"
command -v npm >/dev/null || { echo "npm-run: npm is not installed" >&2; exit 1; }
work="$(mktemp -d)"
port=18690 mport=18692 fail=0
sfx="$RANDOM$RANDOM"
mine="mine-$sfx" pub="pub-$sfx" all="all-$sfx"
trap 'kill "${pid:-}" 2>/dev/null; rm -rf "$work"' EXIT
go build -o "$work/godepot" ./cmd/godepot || exit 1
cat >"$work/settings.json" <<EOT
{"repositories": [
 {"name": "$mine", "format": "npm", "kind": "hosted"},
 {"name": "$pub", "format": "npm", "kind": "proxy", "upstream": {"url": "https://registry.npmjs.org"}},
 {"name": "$all", "format": "npm", "kind": "group", "members": ["$pub", "$mine"], "write_to": "$mine", "claims": ["@corp/", "corp-"]}
], "grants": []}
EOT
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=$mport
acct="npmrun-$RANDOM$RANDOM"
"$work/godepot" migrate >/dev/null || { echo "npm-run: migrate failed" >&2; exit 1; }
"$work/godepot" new-account "$acct" -admin >/dev/null 2>&1 || { echo "npm-run: the account could not be made" >&2; exit 1; }
token="$("$work/godepot" new-token "$acct" -life 24h -repositories '*' -actions read,write,delete 2>/dev/null)" || { echo "npm-run: the token could not be made" >&2; exit 1; }
"$work/godepot" serve >"$work/serve.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/healthz" && break; sleep 0.2; done
report() { printf '%-9s %-8s %s\n' "$1" "$2" "$3"; [ "$2" = ok ] || [ "$2" = skipped ] || fail=1; }
metric() { curl -s "http://127.0.0.1:$mport/metrics" | awk -v k="$1{repo=\"$2\"}" 'index($0, k) == 1 {print $2}'; }
reg="http://127.0.0.1:$port/repository/$all/"
# the token goes in a file of npm's own, as npm wants it; the cache of npm is in the work directory
cat >"$work/npmrc" <<EOT
registry=$reg
//127.0.0.1:$port/repository/$all/:_authToken=$token
audit=false
fund=false
update-notifier=false
EOT
npmc() { npm --userconfig "$work/npmrc" --cache "$work/cache-${CACHE:-1}" "$@"; }

# 1. A package of ours, published to the group (npm sends it to write_to), a second version with a tag, installed from the group.
mkdir -p "$work/demo" "$work/app"
cat >"$work/demo/package.json" <<'EOT'
{"name": "@corp/demo", "version": "1.0.0", "main": "index.js", "description": "a demo", "dependencies": {}}
EOT
echo 'module.exports = () => "hello from godepot"' >"$work/demo/index.js"
out="$(cd "$work/demo" && npmc publish 2>&1)"; rc=$?
if [ $rc -ne 0 ]; then report hosted FAILED "npm publish: $(grep "npm ERR" <<<"$out" | grep -v "complete log" | head -6 | tr "\n" " ")"; else
  (cd "$work/demo" && npm version 1.1.0 --no-git-tag-version >/dev/null && npmc publish --tag next >/dev/null 2>&1)
  echo '{"name": "app", "version": "1.0.0", "private": true}' >"$work/app/package.json"
  out="$(cd "$work/app" && CACHE=2 npmc install @corp/demo 2>&1)"; rc=$?
  got="$(cd "$work/app" && node -e 'console.log(require("@corp/demo")())' 2>&1)"
  ver="$(cd "$work/app" && node -p 'require("@corp/demo/package.json").version' 2>&1)"
  if [ $rc -eq 0 ] && [ "$got" = "hello from godepot" ] && [ "$ver" = "1.0.0" ]; then report hosted ok "npm publish sent @corp/demo 1.0.0 and 1.1.0 (tag next) to the group; npm install took latest (1.0.0) from it: \"$got\""
  else report hosted FAILED "rc $rc, $ver, $got: $(tail -3 <<<"$out" | tr '\n' ' ')"; fi

  # 2. The life of it: dist-tag, deprecate, a version that cannot be published over, unpublish of a version and of the package.
  ok=1; why=""
  tags="$(npmc dist-tag ls @corp/demo 2>&1)"; grep -q 'next: 1.1.0' <<<"$tags" || { ok=0; why="$why tags: $tags;"; }
  npmc dist-tag add @corp/demo@1.1.0 latest >/dev/null 2>&1 || { ok=0; why="$why dist-tag add;"; }
  [ "$(npmc view @corp/demo version 2>&1)" = "1.1.0" ] || { ok=0; why="$why latest not moved;"; }
  npmc deprecate "@corp/demo@1.0.0" "use 1.1" >/dev/null 2>&1 || { ok=0; why="$why deprecate;"; }
  grep -q 'use 1.1' <<<"$(npmc view @corp/demo@1.0.0 deprecated 2>&1)" || { ok=0; why="$why deprecated not shown;"; }
  rp="$(cd "$work/demo" && npm version 1.0.0 --no-git-tag-version --allow-same-version 2>&1; echo '// other bytes' >>index.js; npmc publish 2>&1)"
  grep -qi 'EPUBLISHCONFLICT\|cannot publish over\|409' <<<"$rp" || { ok=0; why="$why republish was not refused: $(grep -v notice <<<"$rp" | head -4 | tr '\n' ' ');"; }
  npmc unpublish "@corp/demo@1.0.0" --force >/dev/null 2>&1 || { ok=0; why="$why unpublish version;"; }
  grep -q '1.0.0' <<<"$(npmc view @corp/demo versions 2>&1)" && { ok=0; why="$why version still listed;"; }
  npmc unpublish "@corp/demo" --force >/dev/null 2>&1 || { ok=0; why="$why unpublish package;"; }
  npmc view @corp/demo version >/dev/null 2>&1 && { ok=0; why="$why package still there;"; }
  if [ $ok = 1 ]; then report lifecycle ok "dist-tag ls/add, deprecate, a second publish of 1.0.0 with other bytes refused, unpublish of a version and of the package"; else report lifecycle FAILED "$why"; fi
fi

# 3. Public packages through the proxy of registry.npmjs.org, twice, the second time with an empty npm cache.
if curl -sf -o /dev/null https://registry.npmjs.org/left-pad; then
  mkdir -p "$work/p1" "$work/p2"
  echo '{"name": "p", "version": "1.0.0", "private": true}' | tee "$work/p1/package.json" >"$work/p2/package.json"
  out="$(cd "$work/p1" && CACHE=3 npmc install left-pad@1.3.0 @isaacs/string-locale-compare@1.1.0 2>&1)"; rc=$?
  b="$(metric godepot_upstream_requests_total "$pub")"
  out2="$(cd "$work/p2" && CACHE=4 npmc install left-pad@1.3.0 @isaacs/string-locale-compare@1.1.0 2>&1)"; rc2=$?
  a="$(metric godepot_upstream_requests_total "$pub")"
  has="$(cd "$work/p2" && node -e 'console.log(require("left-pad")("7", 3, "0"))' 2>&1)"
  if [ $rc -eq 0 ] && [ $rc2 -eq 0 ] && [ "$has" = "007" ] && [ -f "$work/p2/node_modules/@isaacs/string-locale-compare/package.json" ] && [ -n "$b" ] && [ "$b" = "$a" ]; then
    report proxy ok "left-pad and a scoped package installed through the proxy of registry.npmjs.org, the hashes checked; again with an empty npm cache: no request upstream ($b in all)"
  else report proxy FAILED "rc $rc/$rc2, \"$has\", requests $b -> $a: $(tail -3 <<<"$out$out2" | tr '\n' ' ')"; fi
  # 4. A claimed name: not found, and not asked.
  b="$(metric godepot_upstream_requests_total "$pub")"
  out="$(cd "$work/p1" && CACHE=5 npmc install corp-internal 2>&1)"; rc=$?
  out3="$(cd "$work/p1" && CACHE=5 npmc install @corp/ui 2>&1)"; rc3=$?
  a="$(metric godepot_upstream_requests_total "$pub")"
  if [ $rc -ne 0 ] && [ $rc3 -ne 0 ] && grep -q '404' <<<"$out" && [ "$b" = "$a" ]; then report group ok "corp-internal and @corp/ui are not found, and registry.npmjs.org was not asked (claimed)"; else report group FAILED "rc $rc/$rc3; requests $b -> $a: $(tail -3 <<<"$out" | tr '\n' ' ')"; fi
else report proxy skipped "registry.npmjs.org cannot be reached from here"; report group skipped "registry.npmjs.org cannot be reached from here"; fi
exit $fail
