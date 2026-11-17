#!/usr/bin/env bash
# What the tests need on this machine, started and stopped by one command:
#   a rqlite for godepot (127.0.0.1:24801), and a blobgate with its own rqlite (127.0.0.1:24803 and :24802), two nodes that are directories, one store with dedup "tenant".
# blobgate is the image BLOBGATE_IMAGE (for example ghcr.io/zystem/blobgate:0.6.1), or the program BLOBGATE_BIN (or `blobgate` in the PATH).
#   eval "$(tools/test/blobgate-stand.sh start)"   starts them, and sets the GODEPOT_* variables the tests read
#   tools/test/blobgate-stand.sh stop              stops and removes them
set -euo pipefail
dir="${GODEPOT_STAND_DIR:-${TMPDIR:-/tmp}/godepot-bg-stand}"
tenant=godepot secret=a-godepot-secret-16-chars admin=an-admin-token-16-chars
rq=24801 bg_rq=24802 port=24803
rqlite="${RQLITE_IMAGE:-rqlite/rqlite}"

wait_for() { for _ in $(seq 90); do curl -sf "$1" >/dev/null && return 0; sleep 0.5; done; echo "blobgate-stand: $1 does not answer" >&2; return 1; }
env_for_blobgate=(BLOBGATE_RQLITE_URL="http://127.0.0.1:$bg_rq" BLOBGATE_PORT="$port" BLOBGATE_SHIM_PORT=0
  BLOBGATE_TENANTS="$tenant=$secret" BLOBGATE_ADMIN_TOKEN="$admin" BLOBGATE_SEAL_KEY=a-seal-key-16-chars-long)

case "${1:-}" in
start)
  "$0" stop >/dev/null 2>&1 || true
  mkdir -p "$dir/n1" "$dir/n2" "$dir/p1"
  docker run -d --name godepot-rq -p "127.0.0.1:$rq:4001" "$rqlite" -fk -disco-mode= >/dev/null
  docker run -d --name godepot-stand-rq -p "127.0.0.1:$bg_rq:4001" "$rqlite" -fk -disco-mode= >/dev/null
  wait_for "http://127.0.0.1:$rq/readyz"; wait_for "http://127.0.0.1:$bg_rq/readyz"
  if [ -n "${BLOBGATE_IMAGE:-}" ]; then
    nodes='":local:/data/n1"},{"remote":":local:/data/n2"'
    docker run -d --name godepot-stand-bg --network host --read-only --tmpfs /data:rw,mode=1777,size=2g \
      $(printf -- '-e %s ' "${env_for_blobgate[@]}") "$BLOBGATE_IMAGE" >/dev/null
  else
    bin="${BLOBGATE_BIN:-$(command -v blobgate || true)}"
    [ -x "$bin" ] || { echo "blobgate-stand: set BLOBGATE_IMAGE or BLOBGATE_BIN" >&2; exit 1; }
    nodes="\":local:$dir/n1\"},{\"remote\":\":local:$dir/n2\""
    env "${env_for_blobgate[@]}" nohup "$bin" >"$dir/blobgate.log" 2>&1 &
    echo $! >"$dir/pid"
  fi
  wait_for "http://127.0.0.1:$port/readyz"
  curl -sf -X PUT "http://127.0.0.1:$port/v1/admin/stores/main" -H "Authorization: Bearer $admin" \
    -d "{\"kind\":\"mirror\",\"default\":true,\"dedup\":\"tenant\",\"nodes\":[{\"remote\":$nodes}]}" >/dev/null
  # a store that cannot link, for the test that says so
  plain='":local:/data/p1"'; [ -n "${BLOBGATE_IMAGE:-}" ] || plain="\":local:$dir/p1\""
  curl -sf -X PUT "http://127.0.0.1:$port/v1/admin/stores/plain" -H "Authorization: Bearer $admin" \
    -d "{\"kind\":\"mirror\",\"nodes\":[{\"remote\":$plain}]}" >/dev/null
  echo "export GODEPOT_RQLITE_URL=http://127.0.0.1:$rq GODEPOT_BLOBGATE_URL=http://127.0.0.1:$port GODEPOT_BLOBGATE_TENANT=$tenant GODEPOT_BLOBGATE_SECRET=$secret GODEPOT_BLOBGATE_STORE=main"
  ;;
stop)
  [ -f "$dir/pid" ] && kill "$(cat "$dir/pid")" 2>/dev/null || true
  docker rm -f godepot-rq godepot-stand-rq godepot-stand-bg >/dev/null 2>&1 || true
  rm -rf "$dir"
  ;;
*) echo "usage: $0 start|stop" >&2; exit 2 ;;
esac
