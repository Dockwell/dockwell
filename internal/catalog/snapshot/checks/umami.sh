# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# umami's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_umami first, once it's installed,
# and check_umami upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[umami]="3.4.0 ghcr.io/umami-software/umami:3.4.0@sha256:85909afc45bdcda1917394594a087421fdbb05610fded0fa9f6fb861abb2f367"

check_umami() {
  local port said token
  port=$(value PORT)
  umami_sign_in() {
    curl -sS --max-time 15 -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$1\"}" "http://127.0.0.1:$port/api/auth/login"
  }
  said=$(umami_sign_in umami) || fail "Umami doesn't answer on port $port"
  token=$(jq -r '.token // empty' <<<"$said" 2>/dev/null)
  [[ -n "$token" ]] || fail "Umami doesn't sign in as its first steps say: ${said:0:300}"
  if [[ "$1" == first ]]; then
    said=$(curl -fsS --max-time 15 -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
      -d '{"name":"e2e-kept","domain":"example.com"}' "http://127.0.0.1:$port/api/websites") || fail "Umami doesn't add a site"
    kept[umami]=$(jq -r .id <<<"$said")
  fi
  said=$(curl -fsS --max-time 15 -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/websites/${kept[umami]}") ||
    fail "Umami doesn't have the site added before the upgrade"
  [[ "$(jq -r .name <<<"$said")" == e2e-kept ]] || fail "Umami's site reads $said"
  said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    -d '{"username":"admin","password":"not-the-password"}' "http://127.0.0.1:$port/api/auth/login") || fail "Umami's sign-in got no answer"
  [[ "$said" == 401 ]] || fail "Umami answered $said to a wrong password, want 401"
}
