# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# home-assistant's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_home-assistant first, once it's installed,
# and check_home-assistant upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[home-assistant]="2026.10.3 ghcr.io/home-assistant/home-assistant:2026.10.3@sha256:407b89cd11a8cb67d388d21739a226c74acbbebe1ac59e3b83b11b43ce76b98f"

check_home-assistant() {
  local port said
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/onboarding") || fail "Home Assistant doesn't answer on port $port"
  if [[ "$1" == first ]]; then
    jq -e 'any(.step == "user" and .done == false)' <<<"$said" >/dev/null || fail "Home Assistant doesn't ask for its owner first: $said"
    said=$(curl -fsS --max-time 30 -H 'Content-Type: application/json' \
      -d "{\"client_id\":\"http://127.0.0.1:$port/\",\"name\":\"Owner\",\"username\":\"owner\",\"password\":\"E2e-owner-passw0rd\",\"language\":\"en\"}" \
      "http://127.0.0.1:$port/api/onboarding/users") || fail "Home Assistant didn't make its owner"
    [[ -n "$(jq -r '.auth_code // empty' <<<"$said")" ]] || fail "Home Assistant made its owner with no sign-in: $said"
    said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/onboarding") || fail "Home Assistant doesn't answer on port $port"
  fi
  # What it keeps is on its volume, so its owner stays through an upgrade.
  jq -e 'any(.step == "user" and .done == true)' <<<"$said" >/dev/null || fail "Home Assistant has no owner: $said"
  # It trusts the proxy in front, so a request through one is answered, not refused with 400.
  said=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 203.0.113.7' "http://127.0.0.1:$port/manifest.json") ||
    fail "Home Assistant doesn't answer on port $port"
  [[ "$said" == 200 ]] || fail "Home Assistant answers $said through a proxy, want 200"
  docker exec "$(container home-assistant)" grep -q 'use_x_forwarded_for: true' /config/configuration.yaml || fail "Home Assistant's configuration.yaml doesn't trust Dockwell's proxy"
}
