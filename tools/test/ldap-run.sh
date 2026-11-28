#!/usr/bin/env bash
# The proof of the login at a directory (step 8 of the plan, A1, A4), run against the program and a real directory of yours. It changes nothing in the directory. It needs the stand of blobgate-stand.sh,
# curl and python3, and these variables (the names of the users and groups are those of your directory):
#   L_URL        ldaps://dc.example.com:636, or ldap://dc.example.com:389 (then StartTLS is used)
#   L_CA         the certificate to trust (PEM): that of the authority (ca.crt) or the server's own (server.crt)
#   L_CA_BY_API  1: the certificate is not named in the settings but given to the program with PUT /api/v1/ldap/ca (it is removed at the end)
#   L_SERVER_NAME  a name the certificate must have (optional; with a ca.crt it is needed when the URL has a name or address the certificate lacks)
#   L_BIND_PW_BY_FILE=1: the program is given that password in a file (L_BIND_PW_FILE), not in its environment
#   L_BIND_DN    the service account                  L_BIND_PW      its password
#   L_USER_BASE, L_USER_FILTER (with {user}; it must not take disabled people), L_GROUP_BASE, L_GROUP_FILTER (optional)
#   L_USER, L_PW     a person and their password      L_GROUP        a group the person is in (directly, or through others when the filter follows them)
#   L_OTHER_GROUP    a group the person is not in
#   L_REFUSED        names that must not get in with L_PW (a disabled one, one with an expired password; spaces between, optional)
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/ldap-run.sh
# It starts the program on port 19290 (metrics 19292), prints one line for each, and exits 1 when anything failed.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${L_URL:?}" "${L_CA:?}" "${L_BIND_DN:?}" "${L_BIND_PW:?}" "${L_USER_BASE:?}" "${L_USER_FILTER:?}" "${L_GROUP_BASE:?}" "${L_USER:?}" "${L_PW:?}" "${L_GROUP:?}" "${L_OTHER_GROUP:?}"
work="$(mktemp -d)"
port=19290 mport=19292 fail=0
for p in $port $mport; do (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null && { echo "ldap-run: port $p is in use" >&2; exit 1; }; done
trap 'kill "${pid:-}" 2>/dev/null; rm -rf "$work"' EXIT
go build -o "$work/godepot" ./cmd/godepot || exit 1
repo="files-$RANDOM$RANDOM"
python3 - "$repo" >"$work/settings.json" <<'PY'
import json, os, sys
e, repo = os.environ.get, sys.argv[1]
ldap = {"url": e("L_URL"), "bind_dn": e("L_BIND_DN"), "password_env": "L_BIND_PW", "user_base": e("L_USER_BASE"),
        "user_filter": e("L_USER_FILTER"), "group_base": e("L_GROUP_BASE"), "refresh": "2s"}
if e("L_CA_BY_API") != "1": ldap["ca_file"] = os.path.abspath(e("L_CA"))
if e("L_URL").startswith("ldap://"): ldap["start_tls"] = True
if e("L_SERVER_NAME"): ldap["server_name"] = e("L_SERVER_NAME")
if e("L_GROUP_FILTER"): ldap["group_filter"] = e("L_GROUP_FILTER")
print(json.dumps({"repositories": [{"name": repo, "format": "raw", "kind": "hosted"}], "ldap": ldap, "grants": [
  {"group": e("L_GROUP"), "repositories": [repo], "paths": ["mine/**"], "actions": ["read", "write"]},
  {"group": e("L_OTHER_GROUP"), "repositories": [repo], "paths": ["theirs/**"], "actions": ["read", "write"]}]}))
PY
if [ "${L_BIND_PW_BY_FILE:-}" = 1 ]; then
  (umask 077; printf '%s\n' "$L_BIND_PW" >"$work/bind-password")
  secret=(env -u L_BIND_PW "L_BIND_PW_FILE=$work/bind-password")
else
  secret=(env)
fi
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=$mport
"$work/godepot" validate >"$work/validate.log" 2>&1 || { cat "$work/validate.log"; exit 1; }
"$work/godepot" migrate >/dev/null || exit 1
"${secret[@]}" "$work/godepot" serve >"$work/serve.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/healthz" && break; sleep 0.2; done
report() { printf '%-9s %-8s %s\n' "$1" "$2" "$3"; [ "$2" = ok ] || fail=1; }
why() { grep -o '"detail":"[^"]*"' "$work/serve.log" | tail -2 | tr '\n' ' '; }
base="http://127.0.0.1:$port"
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
field() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1],""))' "$1"; }
# the passwords go to curl through its configuration on the standard input, not through its arguments
body() { U="$1" P="$2" python3 -c 'import json,os; print(json.dumps({"account": os.environ["U"], "password": os.environ["P"]}))'; }
login() { body "$1" "$2" | curl -s -X POST --data-binary @- "$base/api/v1/login"; }
lcode() { body "$1" "$2" | code -X POST --data-binary @- "$base/api/v1/login"; }
as() { printf 'user = "%s:%s"\n' "$1" "$(printf '%s' "$2" | sed 's/[\\"]/\\&/g')"; }
echo data >"$work/f"

if [ "${L_CA_BY_API:-}" = 1 ]; then
  admin_name="ldaprun-$RANDOM$RANDOM"
  "$work/godepot" new-account "$admin_name" -admin >/dev/null || exit 1
  at="$("$work/godepot" new-token "$admin_name" -life 1h -repositories '*' -actions admin 2>/dev/null)"
  # the token goes to curl through its configuration, not through its arguments
  adm() { printf 'header = "Authorization: Bearer %s"\n' "$at" | curl -s -K - "$@"; }
  adm -o /dev/null -X DELETE "$base/api/v1/ldap/ca"
  before="$(lcode "$L_USER" "$L_PW")"
  put="$(adm -o "$work/ca.json" -w '%{http_code}' -X PUT --data-binary @"$L_CA" "$base/api/v1/ldap/ca")"
  want="$(openssl x509 -in "$L_CA" -noout -fingerprint -sha256 | sed 's/.*=//; s/://g' | tr 'A-F' 'a-f')"
  if [ "$before" = 503 ] && [ "$put" = 200 ] && grep -qF "$want" "$work/ca.json"; then report trust ok "without the certificate a login is 503; PUT /api/v1/ldap/ca takes it (200) and answers with the SHA-256 that openssl shows"; else report trust FAILED "before=$before put=$put $(head -c 300 "$work/ca.json") $(why)"; fi
  trap 'adm -o /dev/null -X DELETE "$base/api/v1/ldap/ca"; kill "${pid:-}" 2>/dev/null; rm -rf "$work"' EXIT
fi

tok="$(login "$L_USER" "$L_PW" | field token)"
who="$(curl -s -H "Authorization: Bearer $tok" "$base/api/v1/whoami")"
mine="$(code -T "$work/f" -H "Authorization: Bearer $tok" "$base/repository/$repo/mine/a")"
theirs="$(code -T "$work/f" -H "Authorization: Bearer $tok" "$base/repository/$repo/theirs/a")"
admin="$(code -H "Authorization: Bearer $tok" "$base/api/v1/accounts")"
if [[ "$tok" == gd_* ]] && [ "$mine" = 201 ] && [ "$theirs" = 403 ] && [ "$admin" = 403 ]; then report login ok "$L_USER logged in at the directory: the grant of $L_GROUP lets them write (201), that of $L_OTHER_GROUP does not apply (403), the admin API is closed (403)"; else report login FAILED "token=${tok:0:3} mine=$mine theirs=$theirs admin=$admin who=$who $(why)"; fi

basic="$(as "$L_USER" "$L_PW" | code -K - "$base/repository/$repo/mine/a")"
caps="$(as "${L_USER^^}" "$L_PW" | code -K - "$base/repository/$repo/mine/a")"
wrong="$(lcode "$L_USER" "not the password at all")"
if [ "$basic" = 200 ] && [ "$caps" = 200 ] && [ "$wrong" = 401 ]; then report password ok "the directory's password works in basic authentication, also with the name in capitals (200); a wrong one is 401"; else report password FAILED "basic=$basic caps=$caps wrong=$wrong $(why)"; fi

errors() { grep -c '"action":"directory"' "$work/serve.log"; }
errors_before="$(errors)" # the login before the certificate was given is one of them
sleep 3 # longer than "refresh": the next request asks the directory again
again="$(code -H "Authorization: Bearer $tok" "$base/repository/$repo/mine/a")"
if [ "$again" = 200 ] && [ "$(errors)" = "$errors_before" ]; then report refresh ok "after the time of refresh the token still works: the person and the groups were asked again, with no error"; else report refresh FAILED "$again $(why)"; fi

if [ -n "${L_REFUSED:-}" ]; then
  got="" bad=0
  for n in $L_REFUSED; do c="$(lcode "$n" "$L_PW")"; got="$got $n=$c"; [ "$c" = 401 ] || bad=1; done
  if [ $bad = 0 ]; then report refused ok "not let in:$got"; else report refused FAILED "$got $(why)"; fi
fi

if grep -qF -- "$L_PW" "$work/serve.log" || grep -qF -- "$L_BIND_PW" "$work/serve.log" || { [ -n "$tok" ] && grep -qF -- "${tok##*_}" "$work/serve.log"; }; then report log FAILED "a secret is in the log"; else report log ok "neither password, nor the token, is in the log of the program"; fi
exit $fail
