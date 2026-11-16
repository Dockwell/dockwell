# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# hermes-agent's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_hermes-agent first, once it's installed,
# and check_hermes-agent upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[hermes-agent]="0.22.4 nousresearch/hermes-agent:v0.22.4@sha256:7a69201726bc64db2072a1371557137c23312d29705cda2b8ea2a0214a50a4cf"

check_hermes-agent() {
  local port api_port base password key jar said status
  port=$(value PORT)
  api_port=$(value API_PORT)
  base="http://127.0.0.1:$port"
  password=$(generated hermes HERMES_DASHBOARD_BASIC_AUTH_PASSWORD)
  key=$(generated hermes API_SERVER_KEY)
  generated hermes HERMES_DASHBOARD_BASIC_AUTH_SECRET >/dev/null
  said=$(curl -fsS --max-time 15 "$base/api/status") || fail "Hermes's dashboard doesn't answer"
  jq -e '.auth_required and (.auth_providers | index("basic") != null)' <<<"$said" >/dev/null || fail "Hermes's dashboard has no password gate"
  status=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' "$base/api/config") || fail "Hermes's config route doesn't answer"
  [[ "$status" == 401 ]] || fail "Hermes answers $status without a dashboard session, want 401"
  status=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H "Origin: $base" -H 'Content-Type: application/json' \
    -d '{"provider":"basic","username":"admin","password":"not-the-password"}' "$base/auth/password-login") || fail "Hermes's sign-in doesn't answer"
  [[ "$status" == 401 ]] || fail "Hermes answers $status to a wrong dashboard password, want 401"
  jar=$(mktemp)
  said=$(curl -fsS --max-time 15 -c "$jar" -H "Origin: $base" -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg p "$password" '{provider:"basic",username:"admin",password:$p}')" "$base/auth/password-login") || fail "Hermes doesn't take its generated dashboard password"
  jq -e '.ok' <<<"$said" >/dev/null || fail "Hermes didn't sign in"
  said=$(curl -fsS --max-time 15 -b "$jar" "$base/api/auth/me") || fail "Hermes's dashboard session doesn't work"
  jq -e '.provider == "basic"' <<<"$said" >/dev/null || fail "Hermes didn't authenticate its password session"
  if [[ "$1" == first ]]; then
    said=$(curl -fsS --max-time 15 -b "$jar" -X PUT -H "Origin: $base" -H 'Content-Type: application/json' \
      -d '{"config":{"agent":{"max_turns":17}}}' "$base/api/config") || fail "Hermes can't save its settings"
    jq -e '.ok' <<<"$said" >/dev/null || fail "Hermes didn't save its settings"
  fi
  said=$(curl -fsS --max-time 15 -b "$jar" "$base/api/config?include_defaults=false") || fail "Hermes can't read its saved settings"
  jq -e '.agent.max_turns == 17' <<<"$said" >/dev/null || fail "Hermes lost its settings through upgrade"
  status=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer not-the-api-key' "http://127.0.0.1:$api_port/v1/capabilities") || fail "Hermes's API doesn't answer"
  [[ "$status" == 401 ]] || fail "Hermes answers $status to a wrong API key, want 401"
  said=$(curl -fsS --max-time 15 -H "Authorization: Bearer $key" "http://127.0.0.1:$api_port/v1/capabilities") || fail "Hermes doesn't take its generated API key"
  jq -e 'type == "object" and has("endpoints")' <<<"$said" >/dev/null || fail "Hermes's authenticated capabilities have no endpoint table"
  rm -f "$jar"
}
