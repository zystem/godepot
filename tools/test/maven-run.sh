#!/usr/bin/env bash
# The proof of the format maven (step 6 of the plan), run against the program with the real client: `mvn deploy:deploy-file` sends a jar (a release, and a snapshot twice) to a group (it goes to the
# group's write_to), `mvn dependency:get` takes them from the group; every other artifact, plugins included, goes through a proxy of Maven Central (the group is the mirror of everything), the
# second time with an empty local repository sending nothing upstream; a claimed name is not asked of Central. It needs the stand of blobgate-stand.sh, mvn and a JDK, and the network:
#   eval "$(tools/test/blobgate-stand.sh start)"
#   tools/test/maven-run.sh
# It starts the program on port 18790 (metrics 18792), prints one line for each, and exits 1 when anything that ran failed; what needs the network says skipped.
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${GODEPOT_RQLITE_URL:?run the stand first: eval \"\$(tools/test/blobgate-stand.sh start)\"}"
: "${GODEPOT_BLOBGATE_URL:?run the stand first}"
command -v mvn >/dev/null && command -v jar >/dev/null || { echo "maven-run: mvn and a JDK (jar) are needed" >&2; exit 1; }
work="$(mktemp -d)"
port=18790 mport=18792 fail=0
sfx="$RANDOM$RANDOM"
mine="mine-$sfx" pub="central-$sfx" all="all-$sfx"
trap 'kill "${pid:-}" 2>/dev/null; rm -rf "$work"' EXIT
go build -o "$work/godepot" ./cmd/godepot || exit 1
cat >"$work/settings.json" <<EOT
{"repositories": [
 {"name": "$mine", "format": "maven", "kind": "hosted"},
 {"name": "$pub", "format": "maven", "kind": "proxy", "upstream": {"url": "https://repo.maven.apache.org/maven2"}},
 {"name": "$all", "format": "maven", "kind": "group", "members": ["$pub", "$mine"], "write_to": "$mine", "claims": ["com/acme/"]}
], "grants": []}
EOT
export GODEPOT_CONFIG_FILE="$work/settings.json" GODEPOT_PORT=$port GODEPOT_METRICS_PORT=$mport
acct="mvnrun-$RANDOM$RANDOM"
"$work/godepot" migrate >/dev/null || { echo "maven-run: migrate failed" >&2; exit 1; }
"$work/godepot" new-account "$acct" -admin >/dev/null 2>&1 || { echo "maven-run: the account could not be made" >&2; exit 1; }
token="$("$work/godepot" new-token "$acct" -life 24h -repositories '*' -actions read,write,delete 2>/dev/null)" || { echo "maven-run: the token could not be made" >&2; exit 1; }
"$work/godepot" serve >"$work/serve.log" 2>&1 &
pid=$!
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/healthz" && break; sleep 0.2; done
report() { printf '%-9s %-8s %s\n' "$1" "$2" "$3"; [ "$2" = ok ] || [ "$2" = skipped ] || fail=1; }
metric() { curl -s "http://127.0.0.1:$mport/metrics" | awk -v k="$1{repo=\"$2\"}" 'index($0, k) == 1 {print $2}'; }
url="http://127.0.0.1:$port/repository/$all/"
# Every repository is the group: it is the mirror of everything, so the plugins come through it too.
cat >"$work/mvn-settings.xml" <<EOT
<settings>
  <servers><server><id>gd</id><username>$acct</username><password>$token</password></server></servers>
  <mirrors><mirror><id>gd</id><mirrorOf>*</mirrorOf><url>$url</url></mirror></mirrors>
</settings>
EOT
mvnc() { mvn -B -q -s "$work/mvn-settings.xml" -Dmaven.repo.local="$work/m2-${REPO:-1}" "$@"; }

# 1. A jar of ours, a release and a snapshot (twice), deployed to the group; resolved from it with an empty local repository.
mkdir -p "$work/classes" && echo 'class Demo {}' >"$work/Demo.java" && printf 'Main-Class: Demo\n' >"$work/m.txt"
(cd "$work" && jar cf demo.jar Demo.java) 
deploy() { mvnc org.apache.maven.plugins:maven-deploy-plugin:2.8.2:deploy-file -Dfile="$work/demo.jar" -DgroupId=com.acme -DartifactId=demo -Dversion="$1" -Dpackaging=jar -Durl="$url" -DrepositoryId=gd "${@:2}" 2>&1; }
out="$(deploy 1.0)"; rc=$?
if [ $rc -ne 0 ]; then report hosted FAILED "deploy: $(grep -E 'ERROR|Failed' <<<"$out" | head -3 | tr '\n' ' ')"; else
  echo 'changed' >>"$work/Demo.java" && (cd "$work" && jar cf demo.jar Demo.java)
  deploy 2.0-SNAPSHOT >/dev/null; sleep 1.2; echo again >>"$work/Demo.java" && (cd "$work" && jar cf demo.jar Demo.java); deploy 2.0-SNAPSHOT >/dev/null
  # a release deployed again with other bytes is refused
  out2="$(deploy 1.0)"; rc2=$?
  REPO=2 mvnc org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get -Dartifact=com.acme:demo:1.0 -Dtransitive=false >"$work/get1.out" 2>&1; rc3=$?
  REPO=2 mvnc org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get -Dartifact=com.acme:demo:2.0-SNAPSHOT -Dtransitive=false >"$work/get2.out" 2>&1; rc4=$?
  meta="$(curl -s -u "$acct:$token" "${url}com/acme/demo/maven-metadata.xml")"
  snap="$(curl -s -u "$acct:$token" "${url}com/acme/demo/2.0-SNAPSHOT/maven-metadata.xml")"
  if [ $rc2 -ne 0 ] && [ $rc3 -eq 0 ] && [ $rc4 -eq 0 ] && grep -q '<release>1.0</release>' <<<"$meta" && grep -q '<buildNumber>2</buildNumber>' <<<"$snap" && [ -f "$work/m2-2/com/acme/demo/1.0/demo-1.0.jar" ]; then
    report hosted ok "mvn deploy:deploy-file sent 1.0 and 2.0-SNAPSHOT (twice) to the group; 1.0 again was refused; mvn dependency:get took both back; the metadata names release 1.0 and snapshot build 2"
  else report hosted FAILED "rc $rc2/$rc3/$rc4; $(tail -3 "$work/get1.out" | tr '\n' ' ')"; fi
fi

# 2. Everything else through the proxy of Maven Central, twice, the second time with an empty local repository.
if curl -sf -o /dev/null https://repo.maven.apache.org/maven2/commons-io/commons-io/maven-metadata.xml; then
  REPO=3 mvnc org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get -Dartifact=commons-io:commons-io:2.11.0 -Dtransitive=false >"$work/c1.out" 2>&1; rc=$?
  b="$(metric godepot_upstream_requests_total "$pub")"
  rm -rf "$work/m2-3"
  REPO=3 mvnc org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get -Dartifact=commons-io:commons-io:2.11.0 -Dtransitive=false >"$work/c2.out" 2>&1; rc2=$?
  a="$(metric godepot_upstream_requests_total "$pub")"
  if [ $rc -eq 0 ] && [ $rc2 -eq 0 ] && [ -f "$work/m2-3/commons-io/commons-io/2.11.0/commons-io-2.11.0.jar" ] && [ -n "$b" ] && [ "$b" = "$a" ]; then
    report proxy ok "the plugin and commons-io 2.11.0 came through the proxy of Maven Central, each file checked against its checksum file; again with an empty local repository: no request upstream ($b in all)"
  else report proxy FAILED "rc $rc/$rc2, requests $b -> $a: $(grep -E 'ERROR' "$work/c1.out" "$work/c2.out" | head -3 | tr '\n' ' ')"; fi
  # 3. A claimed prefix: not found, and not asked.
  b="$(metric godepot_upstream_requests_total "$pub")"
  REPO=3 mvnc org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get -Dartifact=com.acme:internal:1.0 -Dtransitive=false >"$work/c3.out" 2>&1; rc=$?
  a="$(metric godepot_upstream_requests_total "$pub")"
  if [ $rc -ne 0 ] && [ "$b" = "$a" ]; then report group ok "com.acme:internal is not found, and Maven Central was not asked (claimed prefix com/acme/)"; else report group FAILED "rc $rc; requests $b -> $a"; fi
else report proxy skipped "Maven Central cannot be reached from here"; report group skipped "Maven Central cannot be reached from here"; fi
exit $fail
