#!/usr/bin/env bash
# The proof of cleanup and the trash (step 7 of the plan), run against the program with a real rqlite and blobgate: a raw repository with a policy that removes everything but what a pattern keeps, a
# dry report, then the policy with enforce, run by the program itself in the background; the files are gone, the items are in the trash and still hold their bytes; one is restored with curl, the rest are
# emptied, and the bytes the report said would be freed are gone from the store. It needs the stand of blobgate-stand.sh:
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/cleanup-run.sh
# It starts the program on port 18890 (metrics 18892), prints one line for each, and exits 1 when anything failed.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${GODEPOT_BLOBGATE_URL:?run the stand first}"
work="$(mktemp -d)"
port=18890 mport=18892 fail=0
repo="files-$RANDOM$RANDOM"
trap 'kill "${pid:-}" 2>/dev/null; rm -rf "$work"' EXIT
go build -o "$work/godepot" ./cmd/godepot || exit 1
write_settings() { # $1: enforce
cat >"$work/settings.json" <<EOT
{"repositories": [{"name": "$repo", "format": "raw", "kind": "hosted", "cleanup": {"enforce": $1, "remove_older_days": 0, "keep": ["keep/**"]}}], "grants": []}
EOT
}
write_settings false
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=$mport GODEPOT_COLLECT_SECONDS=1 GODEPOT_CLEANUP_SECONDS=60 GODEPOT_GRACE_SECONDS=60 GODEPOT_TRASH_DAYS=7
acct="cleanrun-$RANDOM$RANDOM"
"$work/godepot" migrate >/dev/null || { echo "cleanup-run: migrate failed" >&2; exit 1; }
"$work/godepot" new-account "$acct" -admin >/dev/null 2>&1 || exit 1
token="$("$work/godepot" new-token "$acct" -life 24h -repositories '*' -actions read,write,delete,admin 2>/dev/null)" || exit 1
start() { "$work/godepot" serve >>"$work/serve.log" 2>&1 & pid=$!; for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/healthz" && break; sleep 0.2; done; }
start
report() { printf '%-9s %-8s %s\n' "$1" "$2" "$3"; [ "$2" = ok ] || fail=1; }
H=(-H "Authorization: Bearer $token")
api() { curl -s "${H[@]}" "$@"; }
base="http://127.0.0.1:$port"
for i in 1 2 3; do head -c $((100000 * i)) /dev/urandom >"$work/f$i"; curl -s -o /dev/null "${H[@]}" -X PUT --data-binary @"$work/f$i" "$base/repository/$repo/old/f$i.bin"; done
head -c 50000 /dev/urandom >"$work/k"; curl -s -o /dev/null "${H[@]}" -X PUT --data-binary @"$work/k" "$base/repository/$repo/keep/k.bin"
cp "$work/f1" "$work/dup"; curl -s -o /dev/null "${H[@]}" -X PUT --data-binary @"$work/dup" "$base/repository/$repo/keep/dup-of-f1.bin" # the bytes of f1, kept by name
dry="$(api "$base/api/v1/repositories/$repo/cleanup")"
read -r n bytes freed <<<"$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print(len(d["remove"]), d["bytes"], d["freed"], flush=True)' "$dry")"
live() { api "$base/api/v1/trash" >/dev/null; curl -s "http://127.0.0.1:$mport/metrics" >/dev/null; python3 - "$GODEPOT_RQLITE_URL" <<'PY'
import json, sys, urllib.request, urllib.parse
q = json.dumps([["SELECT COALESCE(SUM(size),0) FROM blobs"]]).encode()
r = urllib.request.Request(sys.argv[1] + "/db/query?level=strong", q, {"Content-Type": "application/json"})
print(json.load(urllib.request.urlopen(r))["results"][0]["values"][0][0])
PY
}
if [ "$n" = 3 ] && [ "$bytes" = 600000 ] && [ "$freed" = 500000 ]; then report dry ok "the dry report names 3 files of 600000 bytes and says 500000 would be freed (f1's bytes are also kept as keep/dup-of-f1.bin)"; else report dry FAILED "n=$n bytes=$bytes freed=$freed"; fi
[ "$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$base/repository/$repo/old/f1.bin")" = 200 ] || report dry FAILED "the dry report removed a file"
before="$(live)"
# Turn enforce on (SIGHUP re-reads the file) and let the program run the policy by itself.
write_settings true; kill -HUP "$pid"; sleep 1
for _ in $(seq 80); do last="$(api "$base/api/v1/repositories/$repo/cleanup?report=last")"; grep -q '"enforced":true' <<<"$last" && break; sleep 1; done
gone=0; for i in 1 2 3; do [ "$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$base/repository/$repo/old/f$i.bin")" = 404 ] && gone=$((gone+1)); done
kept="$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$base/repository/$repo/keep/k.bin")"
items="$(api "$base/api/v1/trash?repository=$repo")"
ni="$(python3 -c 'import json,sys; print(len(json.loads(sys.argv[1])["items"]))' "$items")"
sleep 3
if grep -q '"enforced":true' <<<"$last" && [ $gone = 3 ] && [ "$kept" = 200 ] && [ "$ni" = 3 ] && [ "$(live)" = "$before" ]; then report enforce ok "the program ran the policy by itself: 3 files gone, keep/ kept, 3 items in the trash, no byte freed yet"; else report enforce FAILED "enforced? gone=$gone kept=$kept items=$ni live=$(live) before=$before"; fi
# Restore f2 with the API, and see it back, byte for byte.
id="$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print([i["id"] for i in d["items"] if i["name"].endswith("f2.bin")][0])' "$items")"
rc="$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -X POST "$base/api/v1/trash/$id/restore")"
if [ "$rc" = 200 ] && curl -s "${H[@]}" "$base/repository/$repo/old/f2.bin" | cmp -s - "$work/f2"; then report restore ok "f2.bin came back from the trash, equal to what was written"; else report restore FAILED "rc $rc"; fi
# Empty the rest; after the grace time the bytes go: 500000 - 200000 (f2 is back) = 300000 (f1 is kept by its twin).
for i in $(python3 -c 'import json,sys; print(" ".join(x["id"] for x in json.loads(sys.argv[1])["items"]))' "$(api "$base/api/v1/trash?repository=$repo")"); do curl -s -o /dev/null "${H[@]}" -X DELETE "$base/api/v1/trash/$i"; done
for _ in $(seq 90); do now="$(live)"; [ "$((before - now))" = 300000 ] && break; sleep 1; done
if [ "$((before - $(live)))" = 300000 ]; then report freed ok "the trash emptied: 300000 bytes gone from the store (f3); f1 stays held by its kept twin, f2 is back"; else report freed FAILED "freed $((before - $(live)))"; fi
exit $fail
