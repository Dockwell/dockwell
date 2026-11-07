#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Starts a built dockwell binary and checks it end to end: health, the API
# over Connect JSON, the sign-in guard and claim link, and the embedded panel
# with its precompressed assets.
#
#   scripts/smoke.sh bin/dockwell
set -euo pipefail

bin=${1:?usage: scripts/smoke.sh path/to/dockwell}
log=$(mktemp)
data=$(mktemp -d)

# A throwaway data directory and ephemeral ports, so the smoke test never
# touches a real install's state, and can run beside another one or make
# budgets. On a port someone else holds, it would test their server, and read
# its own log for what that server did. SMOKE_PORT names a port instead.
"$bin" run --listen "127.0.0.1:${SMOKE_PORT:-0}" --agent-listen 127.0.0.1:0 --data-dir "$data" --log-format json >"$log" 2>&1 &
pid=$!
cleanup() {
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -rf "$log" "$data"
}
trap cleanup EXIT

fail() {
  echo "smoke: $*" >&2
  echo "--- dockwell log" >&2
  cat "$log" >&2
  exit 1
}

addr=
for _ in $(seq 50); do
  addr=$(sed -n 's/.*"listener":"control plane","addr":"\([^"]*\)".*/\1/p' "$log")
  [[ -n "$addr" ]] && curl -fsS "http://$addr/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
[[ -n "$addr" ]] || fail "dockwell run didn't say where it listens within 5 s"
base="http://$addr"
[[ "$(curl -fsS "$base/healthz")" == "ok" ]] || fail "/healthz did not answer ok within 5 s"

version=$(curl -fsS -H 'Content-Type: application/json' -d '{}' "$base/dockwell.v1.SystemService/GetVersion")
[[ "$version" == *'"goVersion"'* ]] || fail "GetVersion returned $version"

# The same procedures answer on REST paths, described by the OpenAPI document.
version=$(curl -fsS "$base/v1/version")
[[ "$version" == *'"goVersion"'* ]] || fail "GET /v1/version returned $version"
openapi=$(curl -fsS "$base/v1/openapi.yaml")
[[ "$openapi" == *$'\nopenapi: 3.'* && "$openapi" == *'/v1/servers:'* ]] || fail "GET /v1/openapi.yaml isn't the OpenAPI document"

# Everything else needs a session, and a new install prints its claim link.
status=$(curl -sS -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{}' "$base/dockwell.v1.ServerService/ListServers")
[[ "$status" == 401 ]] || fail "ListServers without a session answered $status, want 401"
status=$(curl -sS -o /dev/null -w '%{http_code}' "$base/v1/servers")
[[ "$status" == 401 ]] || fail "GET /v1/servers without a session answered $status, want 401"
# A token with a valid checksum reaches the database, which doesn't know it;
# a mistyped one is refused before that.
unknown_token=dwt_ZyIhGOWFtPtZeqwurIPPi3NFp8DmcBefeSZsfyIC8Es1rzinA
status=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $unknown_token" "$base/v1/servers")
[[ "$status" == 401 ]] || fail "GET /v1/servers with an unknown API token answered $status, want 401"
grep -q 'refused an unknown API token' "$log" || fail "an unknown API token wasn't looked up, so its checksum is wrong"
status=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer ${unknown_token%?}x" "$base/v1/servers")
[[ "$status" == 401 ]] || fail "GET /v1/servers with a mistyped API token answered $status, want 401"
grep -q 'refused a malformed API token' "$log" || fail "a mistyped API token wasn't refused as malformed"
# Its output isn't a terminal, as a service's isn't: the journal would keep a
# link, so it prints none, and says to run dockwell claim, which does.
if grep -q '/claim#' "$log"; then
  fail "dockwell run printed a claim link where it isn't a terminal"
fi
grep -q "dockwell claim --data-dir $data" "$log" || fail "dockwell run didn't say to run dockwell claim for a link"
claim=$("$bin" claim --data-dir "$data" 2>&1) || fail "dockwell claim failed: $claim"
grep -q "http://localhost:${addr##*:}/claim#" <<<"$claim" || fail "dockwell claim didn't print a claim link: $claim"
status=$(curl -sS -o /dev/null -w '%{http_code}' -H 'Host: evil.test' "$base/healthz")
[[ "$status" == 421 ]] || fail "a request for another host answered $status, want 421"

# Passkeys need a hostname, so a browser loading a page on 127.0.0.1 goes to localhost.
[[ "$(curl -sS -o /dev/null -w '%{redirect_url}' -H 'Accept: text/html' -H 'Sec-Fetch-Mode: navigate' "$base/claim")" == "http://localhost:${addr##*:}/claim" ]] ||
  fail "a browser's page load on $addr doesn't go to localhost"
panel="http://localhost:${addr##*:}"
resolve=(--resolve "localhost:${addr##*:}:127.0.0.1")

page=$(curl -fsS "${resolve[@]}" -H 'Accept: text/html' "$panel/")
[[ "$page" == *'<div id="root"></div>'* ]] || fail "/ is not the panel; was it built? Run make build."
[[ "$(curl -fsS "${resolve[@]}" -H 'Accept: text/html' "$panel/servers/hz-1")" == "$page" ]] || fail "client routes don't fall back to the panel"

asset=$(grep -o '/assets/index-[^"]*\.js' <<<"$page" | head -1)
[[ -n "$asset" ]] || fail "no entry script in index.html"
headers=$(curl -fsS -o /dev/null -D - -H 'Accept-Encoding: br' "$base$asset")
grep -qi '^content-encoding: br' <<<"$headers" || fail "$asset is not served with Brotli"
grep -qi '^cache-control: public, max-age=31536000, immutable' <<<"$headers" || fail "$asset is not cached as immutable"

csp=$(curl -fsS "${resolve[@]}" -o /dev/null -D - -H 'Accept: text/html' "$panel/" | grep -i '^content-security-policy:' || true)
[[ "$csp" == *"script-src 'self'"* ]] || fail "the panel is served without its Content-Security-Policy"

# The control plane reads each Compose file in this hidden command first.
printf '%s' '{"file":"c2VydmljZXM6IHt9Cg==","variables":{},"cpu":5,"memory":1073741824}' | env -i GOMAXPROCS=2 "$bin" __read-compose ||
  fail "the command that reads a Compose file in a limited child failed"
# Into a variable first: a failing --help would pass this as a pipe, under pipefail.
help=$("$bin" --help 2>&1) || fail "dockwell --help failed: $help"
if [[ "$help" == *__read-compose* ]]; then fail "the command that reads a Compose file in a limited child shows in the help"; fi

echo "smoke: health, API over Connect and REST, the OpenAPI document, sign-in guard and panel all answer on $addr"
