#!/usr/bin/env bash
# The proof of step 4 of the plan, run against the program: the conformance suite of the OCI distribution specification, and the clients that are installed (crane, skopeo, helm, oras, cosign; docker if
# its daemon takes a registry on plain HTTP). It needs the stand of blobgate-stand.sh started and its variables set:
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/oci-run.sh
# It builds the program, starts it on port 18190 with a temporary settings file, runs what it finds, prints one line for each, and stops the program. The suite is downloaded (and built) once into
# $GODEPOT_OCI_CACHE (default $TMPDIR/godepot-oci-cache), at the commit named below. It exits 1 when anything that ran failed.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${GODEPOT_BLOBGATE_URL:?run the stand first}"
spec_commit=a139cc423184af6078077b9b7ee336eddbd03f8f # v1.1.1 of github.com/opencontainers/distribution-spec
cache="${GODEPOT_OCI_CACHE:-${TMPDIR:-/tmp}/godepot-oci-cache}"
work="$(mktemp -d)"
port=18190
fail=0
trap 'kill "${pid:-}" 2>/dev/null; rm -rf "$work"' EXIT

go build -o "$work/godepot" ./cmd/godepot || exit 1
cat >"$work/settings.json" <<'EOF'
{"repositories": [{"name": "conf", "format": "oci", "kind": "hosted"}, {"name": "reg", "format": "oci", "kind": "hosted"}], "grants": []}
EOF
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=0
acct="ocirun-$RANDOM$RANDOM" # a name of its own: the database of the stand may hold the accounts of other runs
"$work/godepot" migrate >/dev/null || { echo "oci-run: migrate failed" >&2; exit 1; }
"$work/godepot" new-account "$acct" -admin >/dev/null 2>&1 || { echo "oci-run: the account could not be made" >&2; exit 1; }
token="$("$work/godepot" new-token "$acct" -life 24h -repositories '*' -actions read,write,delete 2>/dev/null)" || { echo "oci-run: the token could not be made" >&2; exit 1; }
"$work/godepot" serve >"$work/serve.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/v2/" && break; sleep 0.2; done

report() { # name, status, detail
  printf '%-12s %-8s %s\n' "$1" "$2" "$3"
  [ "$2" = ok ] || [ "$2" = skipped ] || fail=1
}

# 1. The conformance suite, all four groups.
if command -v git >/dev/null; then
  if [ ! -x "$cache/conformance.test" ]; then
    mkdir -p "$cache" && rm -rf "$cache/spec" &&
      git clone -q https://github.com/opencontainers/distribution-spec "$cache/spec" &&
      git -C "$cache/spec" checkout -q "$spec_commit" &&
      (cd "$cache/spec/conformance" && go test -c -o "$cache/conformance.test") || rm -f "$cache/conformance.test"
  fi
  if [ -x "$cache/conformance.test" ]; then
    out="$(cd "$cache" && OCI_ROOT_URL="http://127.0.0.1:$port" OCI_NAMESPACE=conf/test OCI_CROSSMOUNT_NAMESPACE=conf/other OCI_USERNAME="$acct" OCI_PASSWORD="$token" \
      OCI_TEST_PULL=1 OCI_TEST_PUSH=1 OCI_TEST_CONTENT_DISCOVERY=1 OCI_TEST_CONTENT_MANAGEMENT=1 OCI_REPORT_DIR=none OCI_HIDE_SKIPPED_WORKFLOWS=1 \
      ./conformance.test 2>&1 | sed 's/\x1b\[[0-9;]*m//g')"
    line="$(grep -E 'Passed.*Failed' <<<"$out" | tail -1)"
    if grep -q ' 0 Failed' <<<"$line"; then report conformance ok "${line# }"; else report conformance FAILED "${line:-no result}"; fi
  else
    report conformance skipped "the suite could not be fetched or built"
  fi
else
  report conformance skipped "git is not installed"
fi

host="127.0.0.1:$port"
export DOCKER_CONFIG="$work/docker"
mkdir -p "$DOCKER_CONFIG"
printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$host" "$(printf '%s:%s' "$acct" "$token" | base64 -w0)" >"$DOCKER_CONFIG/config.json"

# 2. The clients. Each pushes, pulls and compares.
if command -v crane >/dev/null; then
  if crane copy alpine:3.20 "$host/reg/crane/alpine:3.20" --insecure >/dev/null 2>"$work/e" &&
    [ "$(crane digest "$host/reg/crane/alpine:3.20" --insecure 2>>"$work/e")" = "$(crane digest alpine:3.20)" ]; then report crane ok "copy of alpine:3.20 in, the digest of the tag is the same as at the source"; else report crane FAILED "$(tail -1 "$work/e")"; fi
else report crane skipped "not installed"; fi

if command -v skopeo >/dev/null; then
  if skopeo copy --dest-tls-verify=false --dest-creds "$acct:$token" docker://alpine:3.19 "docker://$host/reg/skopeo/alpine:3.19" >/dev/null 2>"$work/e" &&
    skopeo copy --src-tls-verify=false --src-creds "$acct:$token" "docker://$host/reg/skopeo/alpine:3.19" "oci:$work/layout:alpine" >/dev/null 2>>"$work/e"; then report skopeo ok "copy of alpine:3.19 in and out"; else report skopeo FAILED "$(tail -1 "$work/e")"; fi
else report skopeo skipped "not installed"; fi

if command -v helm >/dev/null; then
  (cd "$work" && helm create chart >/dev/null && helm package chart >/dev/null &&
    printf '%s' "$token" | helm registry login "$host" -u "$acct" --password-stdin --plain-http >/dev/null &&
    helm push chart-0.1.0.tgz "oci://$host/reg/charts" --plain-http >/dev/null &&
    mkdir pulled && helm pull "oci://$host/reg/charts/chart" --version 0.1.0 --plain-http -d pulled >/dev/null &&
    cmp pulled/chart-0.1.0.tgz chart-0.1.0.tgz) 2>"$work/e" && report helm ok "a chart pushed and pulled, the file is the same" || report helm FAILED "$(tail -1 "$work/e")"
else report helm skipped "not installed"; fi

oras="$(command -v oras || true)"
if [ -n "$oras" ]; then
  (cd "$work" && echo artifact >art.txt && echo note >note.txt &&
    "$oras" push --plain-http -u "$acct" --password-stdin "$host/reg/art:v1" art.txt:text/plain <<<"$token" >/dev/null &&
    "$oras" attach --plain-http -u "$acct" --password-stdin --artifact-type application/vnd.example.note "$host/reg/art:v1" note.txt:text/plain <<<"$token" >/dev/null &&
    "$oras" discover --plain-http -u "$acct" --password-stdin "$host/reg/art:v1" <<<"$token" | grep -q application/vnd.example.note &&
    "$oras" pull --plain-http -u "$acct" --password-stdin "$host/reg/art:v1" -o out <<<"$token" >/dev/null && cmp out/art.txt art.txt) 2>"$work/e" &&
    report oras ok "an artifact pushed, a note attached (found by the referrers API), pulled" || report oras FAILED "$(tail -1 "$work/e")"
else report oras skipped "not installed"; fi

if command -v cosign >/dev/null && command -v crane >/dev/null; then
  (cd "$work" && export COSIGN_PASSWORD="" && cosign generate-key-pair >/dev/null 2>&1 &&
    crane copy alpine:3.20 "$host/reg/signed/alpine:3.20" --insecure >/dev/null &&
    cosign sign --yes --key cosign.key --allow-insecure-registry --tlog-upload=false "$host/reg/signed/alpine:3.20" >/dev/null 2>&1 &&
    cosign verify --key cosign.pub --allow-insecure-registry --insecure-ignore-tlog "$host/reg/signed/alpine:3.20" >/dev/null 2>&1) 2>"$work/e" && report cosign ok "an image signed with a key and verified" || report cosign FAILED "$(tail -1 "$work/e")"
else report cosign skipped "cosign or crane is not installed"; fi

if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
  if printf '%s' "$token" | docker login "$host" -u "$acct" --password-stdin >/dev/null 2>"$work/e"; then
    docker pull -q alpine:3.20 >/dev/null 2>&1; docker tag alpine:3.20 "$host/reg/docker/alpine:d1"
    if docker push -q "$host/reg/docker/alpine:d1" >/dev/null 2>"$work/e" && docker rmi "$host/reg/docker/alpine:d1" >/dev/null && docker pull -q "$host/reg/docker/alpine:d1" >/dev/null 2>>"$work/e"; then
      report docker ok "push, remove and pull"; else report docker FAILED "$(tail -1 "$work/e")"; fi
  else report docker skipped "the daemon does not take $host on plain HTTP (add it to insecure-registries): $(tail -1 "$work/e")"; fi
else report docker skipped "not installed or no daemon"; fi
if command -v podman >/dev/null; then
  if podman pull -q alpine:3.20 >/dev/null 2>&1 && podman tag alpine:3.20 "$host/reg/podman/alpine:p1" &&
    printf '%s' "$token" | podman login --tls-verify=false "$host" -u "$acct" --password-stdin >/dev/null 2>"$work/e" &&
    podman push -q --tls-verify=false "$host/reg/podman/alpine:p1" >/dev/null 2>>"$work/e" && podman rmi "$host/reg/podman/alpine:p1" >/dev/null && podman pull -q --tls-verify=false "$host/reg/podman/alpine:p1" >/dev/null 2>>"$work/e"; then
    report podman ok "push, remove and pull"; else report podman FAILED "$(tail -1 "$work/e")"; fi
else report podman skipped "not installed"; fi
exit $fail
