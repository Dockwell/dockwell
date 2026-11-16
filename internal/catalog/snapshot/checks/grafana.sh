# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# grafana's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_grafana first, once it's installed,
# and check_grafana upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[grafana]="13.2.3 grafana/grafana:13.2.3@sha256:b28bae15e219c998fb0e0424ed724930cc61b1f61fb404d47c862f9a23f9e572"

check_grafana() {
  local port password said
  port=$(value PORT)
  password=$(generated grafana GF_SECURITY_ADMIN_PASSWORD)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/health") || fail "Grafana doesn't answer on port $port"
  jq -e --arg v "$version" '.database == "ok" and .version == $v' <<<"$said" >/dev/null || fail "Grafana says $said, want $version"
  said=$(curl -fsS --max-time 10 -u "admin:$password" "http://127.0.0.1:$port/api/user") || fail "Grafana doesn't sign in with the password Dockwell generated"
  [[ "$(jq -r .login <<<"$said")" == admin ]] || fail "Grafana signed in as $said"
  if [[ "$1" == first ]]; then
    curl -fsS --max-time 10 -u "admin:$password" -H 'Content-Type: application/json' -d '{"title":"e2e-kept"}' "http://127.0.0.1:$port/api/folders" >/dev/null ||
      fail "Grafana doesn't make a folder"
  fi
  curl -fsS --max-time 10 -u "admin:$password" "http://127.0.0.1:$port/api/folders" | jq -e 'any(.title == "e2e-kept")' >/dev/null ||
    fail "Grafana doesn't have the folder made before the upgrade"
  said=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -u admin:not-the-password "http://127.0.0.1:$port/api/user")
  [[ "$said" == 401 ]] || fail "Grafana answered $said to a wrong password"
}
