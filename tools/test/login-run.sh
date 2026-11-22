#!/usr/bin/env bash
# The proof of the passwords of local accounts (step 8 of the plan, part 1), run against the program: `set-password` reads the password from the standard input; the password is the password of basic
# authentication for the data of the account (not for the admin API); POST /api/v1/login gives a token for it; five wrong passwords lock the account; and the real `npm login` (through a terminal, as a person
# types it) gets a token from the registry and publishes with it. It needs the stand of blobgate-stand.sh, npm and python3:
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/login-run.sh
# It starts the program on port 18990 (metrics 18992), prints one line for each, and exits 1 when anything failed.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${GODEPOT_BLOBGATE_URL:?run the stand first}"
work="$(mktemp -d)"
port=18990 mport=18992 fail=0
repo="npm-$RANDOM$RANDOM"; acct="dev$RANDOM$RANDOM"; admin="root$RANDOM$RANDOM"; pw="$acct has a password"
trap 'kill "${pid:-}" 2>/dev/null; rm -rf "$work"' EXIT
go build -o "$work/godepot" ./cmd/godepot || exit 1
cat >"$work/settings.json" <<EOT
{"repositories": [{"name": "$repo", "format": "npm", "kind": "hosted"}], "grants": [{"account": "$acct", "repositories": ["$repo"], "actions": ["read", "write"]}]}
EOT
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=$mport
"$work/godepot" migrate >/dev/null || exit 1
"$work/godepot" new-account "$acct" >/dev/null 2>&1 || exit 1
"$work/godepot" new-account "$admin" -admin >/dev/null 2>&1 || exit 1
out="$(printf '%s\n' "$pw" | "$work/godepot" set-password "$acct" 2>&1)"
"$work/godepot" serve >"$work/serve.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/healthz" && break; sleep 0.2; done
report() { printf '%-9s %-8s %s\n' "$1" "$2" "$3"; [ "$2" = ok ] || fail=1; }
base="http://127.0.0.1:$port"
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# 1. set-password, basic authentication with the password, login for a token.
[[ "$out" == *"the password of $acct is set"* ]] || report cli FAILED "set-password: $out"
tok="$(curl -s -X POST "$base/api/v1/login" -d "{\"account\": \"$acct\", \"password\": \"$pw\", \"life_seconds\": 3600}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token",""))')"
c_tok="$(code -H "Authorization: Bearer $tok" "$base/repository/$repo/zz")"        # the token is the account's: read is allowed, 404 for a package that is not there
c_basic="$(code -u "$acct:$pw" "$base/repository/$repo/zz")"                        # the password in basic authentication: the same
c_wrong="$(code -u "$acct:not the right password" "$base/repository/$repo/zz")"
c_admin="$(code -u "$acct:$pw" "$base/api/v1/accounts")"                            # the password does not open the admin API
if [[ "$tok" == gd_* ]] && [ "$c_tok" = 404 ] && [ "$c_basic" = 404 ] && [ "$c_wrong" = 401 ] && [ "$c_admin" != 200 ]; then report basic ok "set-password from stdin; login gave a token; the password works in basic authentication for data (404 = allowed, no such package), a wrong one is 401, and the admin API is not opened by it ($c_admin)"; else report basic FAILED "tok=$tok tokrc=$c_tok basic=$c_basic wrong=$c_wrong admin=$c_admin"; fi

# 2. The real npm login, through a terminal, then a publish with the token it wrote.
export HOME="$work/home"; mkdir -p "$HOME" "$work/pkg"
python3 - "$acct" "$pw" "$base/repository/$repo/" >"$work/login.out" <<'PY'
import pty, os, sys, time, select
user, pw, reg = sys.argv[1:4]
pid, fd = pty.fork()
if pid == 0:
    os.execvpe("npm", ["npm", "login", "--registry=" + reg, "--auth-type=legacy"], dict(os.environ))
out = b""; answers = [user.encode() + b"\n", pw.encode() + b"\n", b"me@example.org\n"]
t0 = time.time()
while time.time() - t0 < 40:
    r, _, _ = select.select([fd], [], [], 1)
    if r:
        try: d = os.read(fd, 4096)
        except OSError: break
        if not d: break
        out += d
        last = out.split(b"\n")[-1].lower()
        if answers and (b"username" in last or b"password" in last or b"email" in last):
            os.write(fd, answers.pop(0)); time.sleep(0.3)
print(out.decode(errors="replace"))
PY
echo '{"name":"zz-login","version":"1.0.0"}' >"$work/pkg/package.json"; echo a >"$work/pkg/i.js"
pubout="$(cd "$work/pkg" && npm publish --registry="$base/repository/$repo/" 2>&1)"
if grep -q "Logged in" "$work/login.out" && grep -q "_authToken" "$HOME/.npmrc" && grep -q '+ zz-login@1.0.0' <<<"$pubout"; then report npm ok "npm login (typed at a terminal) got a token and wrote it to .npmrc; npm publish with it worked"; else report npm FAILED "$(tail -3 "$work/login.out" | tr '\n' ' ') / $(tail -2 <<<"$pubout" | tr '\n' ' ')"; fi

# 3. Five wrong passwords lock the account: 429 with Retry-After, also for the right password; a token that was made before is not touched.
for _ in 1 2 3 4 5; do code -X POST "$base/api/v1/login" -d "{\"account\": \"$acct\", \"password\": \"wrong password here\"}" >/dev/null; done
l_code="$(code -X POST "$base/api/v1/login" -d "{\"account\": \"$acct\", \"password\": \"$pw\"}")"
l_ra="$(curl -s -D- -o /dev/null -X POST "$base/api/v1/login" -d "{\"account\": \"$acct\", \"password\": \"$pw\"}" | tr -d '\r' | awk -F': ' 'tolower($1)=="retry-after" {print $2}')"
b_code="$(code -u "$acct:$pw" "$base/repository/$repo/zz")"
t_code="$(code -H "Authorization: Bearer $tok" "$base/repository/$repo/zz")"
if [ "$l_code" = 429 ] && [ -n "$l_ra" ] && [ "$b_code" = 429 ] && [ "$t_code" = 404 ]; then report lock ok "five wrong passwords: 429 (Retry-After $l_ra s) for login and basic authentication, also with the right password; the token made before still works"; else report lock FAILED "login=$l_code ra=$l_ra basic=$b_code token=$t_code"; fi
# The log has no password.
if grep -q "$pw" "$work/serve.log"; then report log FAILED "the password is in the log"; else report log ok "the password is nowhere in the log of the program"; fi
exit $fail
