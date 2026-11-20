#!/usr/bin/env bash
# The proof of step 5 of the plan, run against the program: proxy and group repositories (docs/proxy.md). It needs the stand of blobgate-stand.sh started and its variables set:
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/proxy-run.sh
# It builds the program, starts it on port 18290 (metrics 18292) with a temporary settings file, and runs, printing one line each:
#   PROXY_SKIP_BIG=1 skips the first two (they take minutes)
#   big      a layer of PROXY_BIG_MIB MiB (default 5120) through a false registry that sends PROXY_RATE MiB a second (default 100): the first byte after a short time, the digest equal, the memory of godepot
#   cold     PROXY_CLIENTS clients (default 100) ask a cold layer of PROXY_COLD_MIB MiB (default 512): one fetch
#   hub      crane pulls alpine through a proxy of Docker Hub, then again: no new request to the upstream
#   group    an image pushed to a group goes to its write_to, and a public image is pulled through the same group
#   raw      curl asks a file of a public web server through a raw proxy, twice
# What needs the network or a program that is not installed (crane, curl) says skipped. It exits 1 when anything that ran failed.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${GODEPOT_BLOBGATE_URL:?run the stand first}"
work="$(mktemp -d)"
port=18290 mport=18292 fakeport=18291
sfx="$RANDOM$RANDOM" # repository names of their own: the stand keeps what an earlier run cached, and a run must start cold
up="up-$sfx" hub="hub-$sfx" mine="mine-$sfx" all="all-$sfx" web="web-$sfx"
fail=0
trap 'kill "${pid:-}" 2>/dev/null; rm -rf "$work"' EXIT

go build -o "$work/godepot" ./cmd/godepot || exit 1
go build -o "$work/proxybig" ./tools/proxybig || exit 1
cat >"$work/settings.json" <<EOT
{"repositories": [
 {"name": "$up", "format": "oci", "kind": "proxy", "upstream": {"url": "http://127.0.0.1:$fakeport", "allow_private": true}},
 {"name": "$hub", "format": "oci", "kind": "proxy", "upstream": {"url": "https://registry-1.docker.io"}},
 {"name": "$mine", "format": "oci", "kind": "hosted"},
 {"name": "$all", "format": "oci", "kind": "group", "members": ["$hub", "$mine"], "write_to": "$mine", "claims": ["corp/"]},
 {"name": "$web", "format": "raw", "kind": "proxy", "upstream": {"url": "https://raw.githubusercontent.com"}}
], "grants": []}
EOT
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=$mport
acct="proxyrun-$RANDOM$RANDOM"
"$work/godepot" migrate >/dev/null || { echo "proxy-run: migrate failed" >&2; exit 1; }
"$work/godepot" new-account "$acct" -admin >/dev/null 2>&1 || { echo "proxy-run: the account could not be made" >&2; exit 1; }
token="$("$work/godepot" new-token "$acct" -life 24h -repositories '*' -actions read,write,delete 2>/dev/null)" || { echo "proxy-run: the token could not be made" >&2; exit 1; }
"$work/godepot" serve >"$work/serve.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/v2/" && break; sleep 0.2; done

report() { printf '%-8s %-8s %s\n' "$1" "$2" "$3"; [ "$2" = ok ] || [ "$2" = skipped ] || fail=1; }
metric() { curl -s "http://127.0.0.1:$mport/metrics" | awk -v k="$1{repo=\"$2\"}" 'index($0, k) == 1 {print $2}'; }

# 1. A big layer through a slow upstream: the first byte does not wait for the layer.
if [ -n "${PROXY_SKIP_BIG:-}" ]; then report big skipped "PROXY_SKIP_BIG is set"; report cold skipped "PROXY_SKIP_BIG is set"; else
out="$("$work/proxybig" -listen 127.0.0.1:$fakeport -url "http://127.0.0.1:$port" -token "$token" -name $up/big -mib "${PROXY_BIG_MIB:-5120}" -rate "${PROXY_RATE:-100}" -pid "$pid" 2>&1)" &&
  report big ok "$(grep -E 'cold pull|second pull' <<<"$out" | tr '\n' ' ')" || report big FAILED "$(tail -2 <<<"$out" | tr '\n' ' ')"
# 2. Many clients, one cold layer.
out="$("$work/proxybig" -listen 127.0.0.1:$fakeport -url "http://127.0.0.1:$port" -token "$token" -name $up/cold -mib "${PROXY_COLD_MIB:-512}" -rate "${PROXY_RATE:-100}" -clients "${PROXY_CLIENTS:-100}" -pid "$pid" 2>&1)" &&
  report cold ok "$(tail -1 <<<"$out")" || report cold FAILED "$(tail -2 <<<"$out" | tr '\n' ' ')"
fi

host="127.0.0.1:$port"
export DOCKER_CONFIG="$work/docker"
mkdir -p "$DOCKER_CONFIG"
printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$host" "$(printf '%s:%s' "$acct" "$token" | base64 -w0)" >"$DOCKER_CONFIG/config.json"

# 3. A real registry: Docker Hub.
if command -v crane >/dev/null; then
  if want="$(crane digest alpine:3.20 2>/dev/null)" && [ -n "$want" ]; then
    got="$(crane digest "$host/$hub/library/alpine:3.20" --insecure 2>"$work/e")"
    crane pull "$host/$hub/library/alpine:3.20" "$work/alpine.tar" --insecure >/dev/null 2>>"$work/e"
    before="$(metric godepot_upstream_requests_total "$hub")"
    crane pull "$host/$hub/library/alpine:3.20" "$work/alpine2.tar" --insecure >/dev/null 2>>"$work/e"
    after="$(metric godepot_upstream_requests_total "$hub")"
    if [ "$got" = "$want" ] && [ -s "$work/alpine.tar" ] && cmp -s "$work/alpine.tar" "$work/alpine2.tar" && [ -n "$before" ] && [ "$before" = "$after" ]; then
      report hub ok "alpine:3.20 pulled through the proxy of Docker Hub (digest $got); the second pull sent no request upstream ($before requests in all)"
    else report hub FAILED "digest $got want $want; requests $before -> $after: $(tail -1 "$work/e")"; fi
  else report hub skipped "Docker Hub cannot be reached from here"; fi
  # 4. A group: one address for what is ours and what is public.
  if [ -n "${want:-}" ]; then
    if crane copy alpine:3.19 "$host/$all/corp/app:1" --insecure >/dev/null 2>"$work/e" &&
      [ "$(crane digest "$host/$mine/corp/app:1" --insecure 2>>"$work/e")" = "$(crane digest alpine:3.19)" ] &&
      [ "$(crane digest "$host/$all/corp/app:1" --insecure 2>>"$work/e")" = "$(crane digest alpine:3.19)" ] &&
      [ "$(crane digest "$host/$all/library/alpine:3.20" --insecure 2>>"$work/e")" = "$want" ]; then
      report group ok "crane copy to the group went to its write_to (mine); the same image and a public one are read through the group"
    else report group FAILED "$(tail -1 "$work/e")"; fi
  fi
else report hub skipped "crane is not installed"; report group skipped "crane is not installed"; fi

# 5. A raw proxy of a public web server.
if command -v curl >/dev/null; then
  f=opencontainers/distribution-spec/main/LICENSE
  code="$(curl -s -o "$work/lic1" -w '%{http_code}' -H "Authorization: Bearer $token" "http://$host/repository/$web/$f")"
  if [ "$code" = 200 ]; then
    b="$(metric godepot_upstream_requests_total "$web")"
    curl -s -o "$work/lic2" -H "Authorization: Bearer $token" "http://$host/repository/$web/$f"
    a="$(metric godepot_upstream_requests_total "$web")"
    if cmp -s "$work/lic1" "$work/lic2" && [ -s "$work/lic1" ] && [ -n "$a" ] && [ "$a" = "$b" ]; then report raw ok "a file of raw.githubusercontent.com asked twice: one request upstream"; else report raw FAILED "requests $b -> $a"; fi
  else report raw skipped "answered $code (no network?)"; fi
else report raw skipped "curl is not installed"; fi
exit $fail
