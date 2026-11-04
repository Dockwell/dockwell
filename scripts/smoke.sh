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
addr=127.0.0.1:${SMOKE_PORT:-7699}
base="http://$addr"
log=$(mktemp)
data=$(mktemp -d)

# A throwaway data directory and an ephemeral agent port, so the smoke test
# never touches a real install's state.
"$bin" run --listen "$addr" --agent-listen 127.0.0.1:0 --data-dir "$data" --log-format json >"$log" 2>&1 &
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

for _ in $(seq 50); do
  curl -fsS "$base/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
[[ "$(curl -fsS "$base/healthz")" == "ok" ]] || fail "/healthz did not answer ok within 5 s"

version=$(curl -fsS -H 'Content-Type: application/json' -d '{}' "$base/dockwell.v1.SystemService/GetVersion")
[[ "$version" == *'"goVersion"'* ]] || fail "GetVersion returned $version"

# Everything else needs a session, and a new install prints its claim link.
status=$(curl -sS -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{}' "$base/dockwell.v1.ServerService/ListServers")
[[ "$status" == 401 ]] || fail "ListServers without a session answered $status, want 401"
grep -q "http://localhost:${addr##*:}/claim#" "$log" || fail "dockwell run didn't print a claim link"
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

echo "smoke: health, API, sign-in guard and panel all answer on $addr"
