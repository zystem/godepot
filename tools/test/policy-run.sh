#!/usr/bin/env bash
# The proof of the cooldown and the block lists (step 7 of the plan), run against the program with the real clients and the real registries. For each of npm, pip and go the script asks the registry when two
# versions of a small package were published, sets the cooldown between the two ages so that the newer is held back, and checks what the client does: it takes the older, and a request for the newer by name
# is refused with the words; then a block list names a version and the client does not get it. It needs the stand of blobgate-stand.sh, npm, python3 with venv, go, and the network:
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/policy-run.sh
# It starts the program on port 18990 (metrics 18992), prints one line for each, and exits 1 when anything that ran failed; what needs the network says skipped.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${GODEPOT_BLOBGATE_URL:?run the stand first}"
work="$(mktemp -d)"
port=18990 mport=18992 fail=0
sfx="$RANDOM$RANDOM"
trap 'kill "${pid:-}" 2>/dev/null; chmod -R u+w "$work" 2>/dev/null; rm -rf "$work"' EXIT
go build -o "$work/godepot" ./cmd/godepot || exit 1
report() { printf '%-9s %-8s %s\n' "$1" "$2" "$3"; [ "$2" = ok ] || [ "$2" = skipped ] || fail=1; }
# age_hours TIMESTAMP: how many whole hours ago.
age_hours() { python3 -c 'import sys,datetime as d; t=d.datetime.fromisoformat(sys.argv[1].replace("Z","+00:00")); print(int((d.datetime.now(d.timezone.utc)-t).total_seconds()//3600))' "$1"; }
mid() { echo $(( ($1 + $2) / 2 )); }
net=1
curl -sf -o /dev/null https://registry.npmjs.org/left-pad && curl -sf -o /dev/null https://pypi.org/simple/six/ && curl -sf -o /dev/null https://proxy.golang.org/github.com/google/uuid/@v/list || net=0
if [ $net = 0 ]; then report policy skipped "the public registries cannot be reached from here"; exit 0; fi

# The times of the versions, as the registries say them.
npm_new="$(curl -s https://registry.npmjs.org/left-pad | python3 -c 'import json,sys; print(json.load(sys.stdin)["time"]["1.3.0"])')"
npm_old="$(curl -s https://registry.npmjs.org/left-pad | python3 -c 'import json,sys; print(json.load(sys.stdin)["time"]["1.2.0"])')"
py_new="$(curl -s -H 'Accept: application/vnd.pypi.simple.v1+json' https://pypi.org/simple/six/ | python3 -c 'import json,sys; print([f["upload-time"] for f in json.load(sys.stdin)["files"] if f["filename"]=="six-1.17.0-py2.py3-none-any.whl"][0])')"
py_old="$(curl -s -H 'Accept: application/vnd.pypi.simple.v1+json' https://pypi.org/simple/six/ | python3 -c 'import json,sys; print([f["upload-time"] for f in json.load(sys.stdin)["files"] if f["filename"]=="six-1.16.0-py2.py3-none-any.whl"][0])')"
go_new="$(curl -s https://proxy.golang.org/github.com/google/uuid/@v/v1.6.0.info | python3 -c 'import json,sys; print(json.load(sys.stdin)["Time"])')"
go_old="$(curl -s https://proxy.golang.org/github.com/google/uuid/@v/v1.5.0.info | python3 -c 'import json,sys; print(json.load(sys.stdin)["Time"])')"
cn="$(mid "$(age_hours "$npm_new")" "$(age_hours "$npm_old")")h"; cp="$(mid "$(age_hours "$py_new")" "$(age_hours "$py_old")")h"; cg="$(mid "$(age_hours "$go_new")" "$(age_hours "$go_old")")h"

# Two repositories of each format: one with a cooldown between the two ages, one with a block list.
cat >"$work/settings.json" <<EOT
{"repositories": [
 {"name": "npm-cool-$sfx", "format": "npm", "kind": "proxy", "upstream": {"url": "https://registry.npmjs.org", "cooldown": "$cn"}},
 {"name": "npm-block-$sfx", "format": "npm", "kind": "proxy", "upstream": {"url": "https://registry.npmjs.org", "block": ["left-pad@1.3.0"]}},
 {"name": "pypi-cool-$sfx", "format": "pypi", "kind": "proxy", "upstream": {"url": "https://pypi.org", "file_hosts": ["files.pythonhosted.org"], "cooldown": "$cp"}},
 {"name": "pypi-block-$sfx", "format": "pypi", "kind": "proxy", "upstream": {"url": "https://pypi.org", "file_hosts": ["files.pythonhosted.org"], "block": ["six@1.17.0"]}},
 {"name": "go-cool-$sfx", "format": "go", "kind": "proxy", "anonymous_read": true, "upstream": {"url": "https://proxy.golang.org", "cooldown": "$cg"}},
 {"name": "go-block-$sfx", "format": "go", "kind": "proxy", "anonymous_read": true, "upstream": {"url": "https://proxy.golang.org", "block": ["github.com/google/uuid@v1.6.0"]}}
], "grants": []}
EOT
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=$mport
acct="polrun-$RANDOM$RANDOM"
"$work/godepot" migrate >/dev/null || { echo "policy-run: migrate failed" >&2; exit 1; }
"$work/godepot" new-account "$acct" -admin >/dev/null 2>&1 || exit 1
token="$("$work/godepot" new-token "$acct" -life 24h -repositories '*' -actions read 2>/dev/null)" || exit 1
"$work/godepot" serve >"$work/serve.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/healthz" && break; sleep 0.2; done
base="http://127.0.0.1:$port/repository"

# npm
npmc() { npm --registry "$base/$1/" --//127.0.0.1:$port/repository/$1/:_authToken="$token" --cache "$work/npmcache-$1" --audit=false --fund=false --update-notifier=false "${@:2}"; }
mkdir -p "$work/n1" "$work/n2"; echo '{"name":"p","version":"1.0.0","private":true}' | tee "$work/n1/package.json" >"$work/n2/package.json"
(cd "$work/n1" && npmc "npm-cool-$sfx" install left-pad >/dev/null 2>&1); got="$(cd "$work/n1" && node -p 'require("left-pad/package.json").version' 2>&1)"
body="$(curl -s -H "Authorization: Bearer $token" "$base/npm-cool-$sfx/left-pad/-/left-pad-1.3.0.tgz")"; code="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "$base/npm-cool-$sfx/left-pad/-/left-pad-1.3.0.tgz")"
(cd "$work/n2" && npmc "npm-block-$sfx" install left-pad >/dev/null 2>&1); got2="$(cd "$work/n2" && node -p 'require("left-pad/package.json").version' 2>&1)"
bcode="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "$base/npm-block-$sfx/left-pad/-/left-pad-1.3.0.tgz")"
if [ "$got" = "1.2.0" ] && [ "$code" = 403 ] && grep -q 'will be served from' <<<"$body" && [ "$got2" = "1.2.0" ] && [ "$bcode" = 403 ]; then report npm ok "cooldown $cn: npm install left-pad took 1.2.0 (the latest, 1.3.0, is held back); 1.3.0 by name is 403 \"...will be served from...\"; with the block list the same"; else report npm FAILED "cool=$got/$code block=$got2/$bcode: $body"; fi

# pip
if python3 -m venv "$work/venv" 2>/dev/null; then
  pipc() { "$work/venv/bin/pip" download -q --no-cache-dir --no-deps -d "$2" --index-url "http://$acct:$token@127.0.0.1:$port/repository/$1/simple/" --trusted-host 127.0.0.1 "${@:3}" 2>&1; }
  pipc "pypi-cool-$sfx" "$work/p1" six >/dev/null; f1="$(ls "$work/p1" 2>/dev/null)"
  out="$(pipc "pypi-cool-$sfx" "$work/p1b" six==1.17.0)"; rc=$?
  pipc "pypi-block-$sfx" "$work/p2" six >/dev/null; f2="$(ls "$work/p2" 2>/dev/null)"
  if [[ "$f1" == six-1.16.0* ]] && [ $rc -ne 0 ] && [[ "$f2" == six-1.16.0* ]]; then report pip ok "cooldown $cp: pip took six 1.16.0 (1.17.0 is held back and not in the index); pip install six==1.17.0 fails; with the block list the same"; else report pip FAILED "cool=$f1 rc=$rc block=$f2: $(tail -2 <<<"$out" | tr '\n' ' ')"; fi
else report pip skipped "python3 venv is not available"; fi

# go
gc() { GOFLAGS=-mod=mod GOPATH="$work/gopath-$1" GOMODCACHE="$work/gomod-$1" GOPROXY="http://127.0.0.1:$port/repository/$1" GONOSUMDB='*' GONOSUMCHECK=1 GOFLAGS='-mod=mod' GOSUMDB=off "${@:2}"; }
mkdir -p "$work/g1" "$work/g2"
for d in g1 g2; do printf 'module demo\n\ngo 1.20\n' >"$work/$d/go.mod"; done
(cd "$work/g1" && gc "go-cool-$sfx" go get github.com/google/uuid >/dev/null 2>&1); v1="$(cd "$work/g1" && gc "go-cool-$sfx" go list -m -f '{{.Version}}' github.com/google/uuid 2>/dev/null)"
(cd "$work/g2" && gc "go-block-$sfx" go get github.com/google/uuid >/dev/null 2>&1); v2="$(cd "$work/g2" && gc "go-block-$sfx" go list -m -f '{{.Version}}' github.com/google/uuid 2>/dev/null)"
gcode="$(curl -s -o /dev/null -w '%{http_code}' "$base/go-cool-$sfx/github.com/google/uuid/@v/v1.6.0.info")"
if [ "$v1" = "v1.5.0" ] && [ "$gcode" = 403 ] && [ "$v2" = "v1.5.0" ]; then report go ok "cooldown $cg: go get github.com/google/uuid took $v1, not v1.6.0, the latest (held back; by name 403); with the block list: $v2"; else report go FAILED "cool=$v1/$gcode block=$v2"; fi
exit $fail
