#!/usr/bin/env bash
# The proof of the format pypi (step 6 of the plan), run against the program with the real clients: `twine` uploads a wheel to a group (it goes to the group's write_to), `pip` installs it from the
# group; a public package (six) is installed through a proxy of pypi.org, the second time sending nothing upstream; a claimed name is not asked of pypi.org. It needs the stand of blobgate-stand.sh,
# python3 with venv, and the network (twine is installed into a temporary virtual environment):
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/pypi-run.sh
# It starts the program on port 18590 (metrics 18592), prints one line for each, and exits 1 when anything that ran failed; what needs the network says skipped.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${GODEPOT_BLOBGATE_URL:?run the stand first}"
work="$(mktemp -d)"
port=18590 mport=18592 fail=0
sfx="$RANDOM$RANDOM"
mine="mine-$sfx" pub="pub-$sfx" all="all-$sfx"
trap 'kill "${pid:-}" 2>/dev/null; rm -rf "$work"' EXIT
go build -o "$work/godepot" ./cmd/godepot || exit 1
cat >"$work/settings.json" <<EOT
{"repositories": [
 {"name": "$mine", "format": "pypi", "kind": "hosted"},
 {"name": "$pub", "format": "pypi", "kind": "proxy", "upstream": {"url": "https://pypi.org", "file_hosts": ["files.pythonhosted.org"]}},
 {"name": "$all", "format": "pypi", "kind": "group", "members": ["$pub", "$mine"], "write_to": "$mine", "claims": ["corp-"]}
], "grants": []}
EOT
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=$mport
acct="pyrun-$RANDOM$RANDOM"
"$work/godepot" migrate >/dev/null || { echo "pypi-run: migrate failed" >&2; exit 1; }
"$work/godepot" new-account "$acct" -admin >/dev/null 2>&1 || { echo "pypi-run: the account could not be made" >&2; exit 1; }
token="$("$work/godepot" new-token "$acct" -life 24h -repositories '*' -actions read,write,delete 2>/dev/null)" || { echo "pypi-run: the token could not be made" >&2; exit 1; }
"$work/godepot" serve >"$work/serve.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/healthz" && break; sleep 0.2; done
report() { printf '%-8s %-8s %s\n' "$1" "$2" "$3"; [ "$2" = ok ] || [ "$2" = skipped ] || fail=1; }
metric() { curl -s "http://127.0.0.1:$mport/metrics" | awk -v k="$1{repo=\"$2\"}" 'index($0, k) == 1 {print $2}'; }
index="http://$acct:$token@127.0.0.1:$port/repository/$all/simple/"

if ! python3 -m venv "$work/venv" 2>/dev/null || ! "$work/venv/bin/pip" install -q twine >"$work/pipinstall.log" 2>&1; then
  report hosted skipped "twine could not be installed into a virtual environment (no network?)"
else
  # 1. A wheel of ours, built by hand, uploaded by twine through the group, installed by pip from the group.
  mkdir -p "$work/dist"
  python3 - "$work" <<'PY'
import sys, zipfile, hashlib, base64
w = sys.argv[1]
files = {
  "godepot_demo/__init__.py": b'def hello():\n    return "hello from godepot"\n',
  "godepot_demo-1.0.dist-info/METADATA": b"Metadata-Version: 2.1\nName: godepot-demo\nVersion: 1.0\nRequires-Python: >=3.8\nSummary: a demo\n\nA demo.\n",
  "godepot_demo-1.0.dist-info/WHEEL": b"Wheel-Version: 1.0\nGenerator: by hand\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
}
rec = []
for n, b in files.items():
    d = base64.urlsafe_b64encode(hashlib.sha256(b).digest()).rstrip(b"=").decode()
    rec.append("%s,sha256=%s,%d" % (n, d, len(b)))
rec.append("godepot_demo-1.0.dist-info/RECORD,,")
files["godepot_demo-1.0.dist-info/RECORD"] = ("\n".join(rec) + "\n").encode()
with zipfile.ZipFile(w + "/dist/godepot_demo-1.0-py3-none-any.whl", "w") as z:
    for n, b in files.items():
        z.writestr(n, b)
PY
  out="$("$work/venv/bin/twine" upload --non-interactive --repository-url "http://127.0.0.1:$port/repository/$all/legacy/" -u "$acct" -p "$token" "$work"/dist/* 2>&1)"; rc=$?
  if [ $rc -eq 0 ]; then
    "$work/venv/bin/pip" install -q --no-cache-dir --target "$work/site" --index-url "$index" --trusted-host 127.0.0.1 godepot-demo >"$work/pip.out" 2>&1
    got="$(PYTHONPATH="$work/site" python3 -c 'import godepot_demo; print(godepot_demo.hello())' 2>&1)"
    if [ "$got" = "hello from godepot" ]; then report hosted ok "twine uploaded a wheel to the group (write_to), pip installed it from the group: \"$got\""; else report hosted FAILED "pip: $(tail -2 "$work/pip.out" | tr '\n' ' ') / $got"; fi
  else report hosted FAILED "twine: $(tail -3 <<<"$out" | tr '\n' ' ')"; fi

  # 2. A public package through the proxy of pypi.org, twice, with no pip cache.
  if curl -sf -o /dev/null https://pypi.org/simple/six/; then
    "$work/venv/bin/pip" download -q --no-cache-dir --no-deps -d "$work/dl1" --index-url "$index" --trusted-host 127.0.0.1 six==1.16.0 >"$work/pip1.out" 2>&1
    b="$(metric godepot_upstream_requests_total "$pub")"
    "$work/venv/bin/pip" download -q --no-cache-dir --no-deps -d "$work/dl2" --index-url "$index" --trusted-host 127.0.0.1 six==1.16.0 >"$work/pip2.out" 2>&1
    a="$(metric godepot_upstream_requests_total "$pub")"
    if [ -n "$(ls "$work/dl1" 2>/dev/null)" ] && cmp -s "$work/dl1"/* "$work/dl2"/* && [ -n "$b" ] && [ "$b" = "$a" ]; then
      report proxy ok "six==1.16.0 downloaded through the proxy of pypi.org (files.pythonhosted.org is the file host), the hash checked; again with no pip cache: no request upstream ($b in all)"
    else report proxy FAILED "$(tail -2 "$work/pip1.out" | tr '\n' ' ') requests $b -> $a"; fi
    # 3. A claimed prefix: not found, and not asked.
    b="$(metric godepot_upstream_requests_total "$pub")"
    "$work/venv/bin/pip" download -q --no-cache-dir --no-deps -d "$work/dl3" --index-url "$index" --trusted-host 127.0.0.1 corp-internal >"$work/pip3.out" 2>&1; rc=$?
    a="$(metric godepot_upstream_requests_total "$pub")"
    if [ $rc -ne 0 ] && [ "$b" = "$a" ]; then report group ok "corp-internal is not found, and pypi.org was not asked (claimed prefix)"; else report group FAILED "rc $rc; requests $b -> $a"; fi
  else report proxy skipped "pypi.org cannot be reached from here"; report group skipped "pypi.org cannot be reached from here"; fi
fi
exit $fail
