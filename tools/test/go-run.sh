#!/usr/bin/env bash
# The proof of the format go (step 6 of the plan), run against the program with the real go command: a module published to a hosted repository and built from it, a public module through a proxy of
# proxy.golang.org (the second download, with an empty module cache, sends nothing upstream), and a group whose claim keeps a name of ours from the public proxy. It needs the stand of blobgate-stand.sh:
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/go-run.sh
# It starts the program on port 18490 (metrics 18492). It prints one line for each and exits 1 when anything that ran failed; what needs the network or a tool that is missing says skipped.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${GODEPOT_BLOBGATE_URL:?run the stand first}"
work="$(mktemp -d)"
port=18490 mport=18492 fail=0
sfx="$RANDOM$RANDOM"
mods="mods-$sfx" pub="pub-$sfx" all="all-$sfx"
trap 'kill "${pid:-}" 2>/dev/null; chmod -R u+w "$work" 2>/dev/null; rm -rf "$work"' EXIT
go build -o "$work/godepot" ./cmd/godepot || exit 1
cat >"$work/settings.json" <<EOT
{"repositories": [
 {"name": "$mods", "format": "go", "kind": "hosted"},
 {"name": "$pub", "format": "go", "kind": "proxy", "anonymous_read": true, "upstream": {"url": "https://proxy.golang.org"}},
 {"name": "$all", "format": "go", "kind": "group", "anonymous_read": true, "members": ["$pub", "$mods"], "write_to": "$mods", "claims": ["corp.example.com/"]}
], "grants": []}
EOT
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=$mport
acct="gorun-$RANDOM$RANDOM"
"$work/godepot" migrate >/dev/null || { echo "go-run: migrate failed" >&2; exit 1; }
"$work/godepot" new-account "$acct" -admin >/dev/null 2>&1 || { echo "go-run: the account could not be made" >&2; exit 1; }
token="$("$work/godepot" new-token "$acct" -life 24h -repositories '*' -actions read,write,delete 2>/dev/null)" || { echo "go-run: the token could not be made" >&2; exit 1; }
"$work/godepot" serve >"$work/serve.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/healthz" && break; sleep 0.2; done
report() { printf '%-8s %-8s %s\n' "$1" "$2" "$3"; [ "$2" = ok ] || [ "$2" = skipped ] || fail=1; }
metric() { curl -s "http://127.0.0.1:$mport/metrics" | awk -v k="$1{repo=\"$2\"}" 'index($0, k) == 1 {print $2}'; }
proxy="http://127.0.0.1:$port/repository" # no credentials: the go command refuses to send them over plain HTTP, so these repositories are readable by anyone (anonymous_read); a publish uses the token with curl
genv() { env GOPATH="$work/gopath$1" GOMODCACHE="$work/modcache$1" GOFLAGS=-mod=mod GOTOOLCHAIN=local "${@:2}"; }

# The go command sends no credentials over HTTP (it says "refusing to pass credentials to insecure URL"): reading with a token needs TLS in front, which is not run here.
# 1. A module of ours: zipped as the go command zips it, published, then built by a consumer from the group.
mkdir -p "$work/hello"
python3 - "$work" <<'PY'
import sys, zipfile
w = sys.argv[1]
with zipfile.ZipFile(w + "/hello.zip", "w") as z:
    z.writestr("example.com/hello@v1.0.0/go.mod", "module example.com/hello\n\ngo 1.21\n")
    z.writestr("example.com/hello@v1.0.0/hello.go", 'package hello\n\nfunc Greeting() string { return "hello from godepot" }\n')
PY
code="$(curl -s -o "$work/put.out" -w '%{http_code}' -X PUT -H "Authorization: Bearer $token" --data-binary @"$work/hello.zip" "http://127.0.0.1:$port/repository/$all/example.com/hello/@v/v1.0.0.zip")"
mkdir -p "$work/app" && cat >"$work/app/go.mod" <<'EOT'
module example.com/app

go 1.21
EOT
cat >"$work/app/main.go" <<'EOT'
package main

import (
	"fmt"

	"example.com/hello"
)

func main() { fmt.Println(hello.Greeting()) }
EOT
if [ "$code" = 201 ]; then
  out="$(cd "$work/app" && genv 1 GOPROXY="$proxy/$all" GOSUMDB=off go run . 2>"$work/err")"
  if [ "$out" = "hello from godepot" ]; then report hosted ok "a module zipped, published to the group (write_to), built by go run from the group: \"$out\""; else report hosted FAILED "go run said: $out $(tail -2 "$work/err")"; fi
else report hosted FAILED "the publish answered $code: $(cat "$work/put.out")"; fi

# 2. A public module through the proxy, with the real checksum database (the proxy answers 404 to its path, and go asks the database itself).
if curl -sf -o /dev/null https://proxy.golang.org/github.com/google/uuid/@v/list; then
  mkdir -p "$work/app2" && cat >"$work/app2/go.mod" <<'EOT'
module example.com/app2

go 1.21
EOT
  cat >"$work/app2/main.go" <<'EOT'
package main

import (
	"fmt"

	"github.com/google/uuid"
)

func main() { fmt.Println(len(uuid.NewString())) }
EOT
  out="$(cd "$work/app2" && genv 2 GOPROXY="$proxy/$pub" go run . 2>"$work/err")"
  b="$(metric godepot_upstream_requests_total "$pub")"
  out2="$(cd "$work/app2" && genv 3 GOPROXY="$proxy/$pub" go run . 2>>"$work/err")"
  a="$(metric godepot_upstream_requests_total "$pub")"
  if [ "$out" = 36 ] && [ "$out2" = 36 ] && [ -n "$b" ] && [ "$b" = "$a" ]; then report proxy ok "github.com/google/uuid through the proxy of proxy.golang.org with the real checksum database; again with an empty module cache: no request upstream ($b in all)"; else report proxy FAILED "go run said: $out / $out2 $(tail -2 "$work/err"); requests $b -> $a"; fi
  # 3. The claim: a name under a claimed prefix is not asked of the public proxy, even though the group lists it first.
  b="$(metric godepot_upstream_requests_total "$pub")"
  out="$(cd "$work/app2" && genv 4 GOPROXY="$proxy/$all" GOSUMDB=off go mod download corp.example.com/internal@v1.0.0 2>&1)"; rc=$?
  a="$(metric godepot_upstream_requests_total "$pub")"
  if [ $rc -ne 0 ] && [ "${b:-0}" = "${a:-0}" ]; then report group ok "corp.example.com/internal is not found, and the public proxy was not asked (claimed prefix)"; else report group FAILED "rc $rc; requests ${b:-0} -> ${a:-0}: $out"; fi
else report proxy skipped "proxy.golang.org cannot be reached from here"; report group skipped "proxy.golang.org cannot be reached from here"; fi
exit $fail
