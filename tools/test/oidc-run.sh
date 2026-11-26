#!/usr/bin/env bash
# The proof of the exchange of a CI job's token (step 8 of the plan, A3), run against the program. The issuer is not our code: `openssl` makes its key and signs the job's tokens and a web server of
# python3 publishes the discovery document and the keys. A token is exchanged for one of godepot, which publishes a file with it; tokens that are not good are refused. The last line reads the keys of the
# real issuer of GitHub Actions (it needs the network): a token that names it and has a real key id but is signed by us must be refused as not signed right, which shows that the keys were read.
# It needs the stand of blobgate-stand.sh, openssl, python3 and curl:
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/oidc-run.sh
# It starts the program on port 19190 (metrics 19192) and the issuer on 19191, prints one line for each, and exits 1 when anything that ran failed; what needs the network says skipped.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${GODEPOT_BLOBGATE_URL:?run the stand first}"
work="$(mktemp -d)"
port=19190 iport=19191 mport=19192 fail=0
for p in $port $iport $mport; do (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null && { echo "oidc-run: port $p is in use" >&2; exit 1; }; done
repo="files-$RANDOM$RANDOM"; acct="ci$RANDOM$RANDOM"
trap 'kill "${pid:-}" "${ipid:-}" 2>/dev/null; rm -rf "$work"' EXIT
go build -o "$work/godepot" ./cmd/godepot || exit 1
issuer="http://127.0.0.1:$iport"
github="https://token.actions.githubusercontent.com"
b64() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

# The issuer: a key of its own, the discovery document and the keys.
openssl genrsa -out "$work/key.pem" 2048 2>/dev/null
n="$(openssl rsa -in "$work/key.pem" -noout -modulus | cut -d= -f2 | python3 -c 'import sys,base64; print(base64.urlsafe_b64encode(bytes.fromhex(sys.stdin.read().strip())).decode().rstrip("="))')"
mkdir -p "$work/www/.well-known"
printf '{"issuer":"%s","jwks_uri":"%s/keys"}' "$issuer" "$issuer" >"$work/www/.well-known/openid-configuration"
printf '{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"k1","n":"%s","e":"AQAB"}]}' "$n" >"$work/www/keys"
python3 -m http.server "$iport" --bind 127.0.0.1 --directory "$work/www" >"$work/issuer.log" 2>&1 &
ipid=$!

# sign KID CLAIMS-JSON: a job token signed by openssl.
sign() {
  local signed
  signed="$(printf '{"alg":"RS256","typ":"JWT","kid":"%s"}' "$1" | b64).$(printf '%s' "$2" | b64)"
  printf '%s.%s' "$signed" "$(printf '%s' "$signed" | openssl dgst -sha256 -sign "$work/key.pem" | b64)"
}
now="$(date +%s)"
claims() { printf '{"iss":"%s","aud":"godepot","sub":"repo:%s:ref:refs/heads/main","repository":"%s","ref":"refs/heads/main","iat":%d,"exp":%d}' "$issuer" "$1" "$1" "$now" "$((now + ${2:-300}))"; }

cat >"$work/settings.json" <<EOT
{"repositories": [{"name": "$repo", "format": "raw", "kind": "hosted"}],
 "grants": [{"account": "$acct", "repositories": ["$repo"], "actions": ["read", "write"]}],
 "issuers": [{"name": "local", "url": "$issuer", "audience": "godepot", "allow_private": true},
             {"name": "github", "url": "$github", "audience": "godepot"}],
 "trusts": [{"issuer": "local", "account": "$acct", "claims": {"repository": "corp/app", "ref": "refs/heads/main"}, "paths": ["releases/**"]}]}
EOT
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=$mport
"$work/godepot" migrate >/dev/null || exit 1
"$work/godepot" new-account "$acct" -robot >/dev/null 2>&1 || exit 1
"$work/godepot" serve >"$work/serve.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/healthz" && curl -s -o /dev/null "$issuer/keys" && break; sleep 0.2; done
report() { printf '%-9s %-8s %s\n' "$1" "$2" "$3"; [ "$2" = ok ] || [ "$2" = skipped ] || fail=1; }
base="http://127.0.0.1:$port"
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
field() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1],""))' "$1"; }
exchange() { curl -s -X POST "$base/api/v1/oidc/token" -d "{\"token\": \"$1\"}"; }
xcode() { code -X POST "$base/api/v1/oidc/token" -d "{\"token\": \"$1\"}"; }

# 1. The exchange, and what the token it gives may do.
job="$(sign k1 "$(claims corp/app)")"
answer="$(exchange "$job")"
tok="$(field token <<<"$answer")"
left=$(($(field expires_at <<<"$answer" || echo 0) - $(date +%s)))
c_put="$(code -T "$work/key.pem" -H "Authorization: Bearer $tok" "$base/repository/$repo/releases/app-1.0")"
c_out="$(code -T "$work/key.pem" -H "Authorization: Bearer $tok" "$base/repository/$repo/other/x")"
c_adm="$(code -H "Authorization: Bearer $tok" "$base/api/v1/accounts")"
if [[ "$tok" == gd_* ]] && [ "$left" -gt 840 ] && [ "$left" -le 900 ] && [ "$c_put" = 201 ] && [ "$c_out" = 403 ] && [ "$c_adm" = 403 ]; then report exchange ok "a token signed by openssl gave a godepot token for $left s; it published releases/app-1.0 (201), not other/x (403), and opens no admin call (403)"; else report exchange FAILED "left=$left put=$c_put out=$c_out admin=$c_adm $(grep -o '"detail":"[^"]*"' "$work/serve.log" | tail -2)"; fi

# 2. What is refused.
other="$(xcode "$(sign k1 "$(claims evil/app)")")"
old="$(xcode "$(sign k1 "$(claims corp/app -600)")")"
cut="$(xcode "${job%????}AAAA")"
if [ "$other" = 403 ] && [ "$old" = 401 ] && [ "$cut" = 401 ]; then report refused ok "a job of another repository 403, an expired token 401, a changed signature 401"; else report refused FAILED "other=$other old=$old cut=$cut"; fi

# 3. The log has the exchange and no token.
if grep -q 'oidc.exchange' "$work/serve.log" && ! grep -qF "$job" "$work/serve.log" && ! grep -qF "${tok##*_}" "$work/serve.log"; then report log ok "the audit lines of oidc.exchange are there; neither the job's token nor godepot's is in the log"; else report log FAILED "see the log"; fi

# 4. The real issuer of GitHub Actions: its discovery document and keys are read.
kid="$(curl -sf -m 10 "$github/.well-known/jwks" | jq -r '.keys[0].kid' 2>/dev/null)"
if [ -z "$kid" ]; then report github skipped "$github cannot be reached from here"
else
  forged="$(sign "$kid" "$(printf '{"iss":"%s","aud":"godepot","sub":"x","exp":%d}' "$github" "$((now + 300))")")"
  c="$(xcode "$forged")"
  if [ "$c" = 401 ] && grep -q 'the signature is not right' "$work/serve.log"; then report github ok "the keys of GitHub's issuer were read (key $kid); a token with that key id signed by us is 401, \"the signature is not right\""; else report github FAILED "code=$c $(grep -o '"detail":"[^"]*"' "$work/serve.log" | tail -1)"; fi
fi
exit $fail
