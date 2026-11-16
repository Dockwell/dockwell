# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# n8n's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_n8n first, once it's installed,
# and check_n8n upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[n8n]="2.44.6 n8nio/n8n:2.44.6@sha256:d02752c6ab446ae9db8deec2f9cc887c378f77cdcad31650814df936b7386dfb"

check_n8n() {
  local port said owner
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/healthz/readiness") || fail "n8n isn't ready on port $port, with its database"
  [[ "$(jq -r .status <<<"$said")" == ok ]] || fail "n8n says it's $said"
  owner='{"email":"owner@example.com","password":"E2e-owner-passw0rd"}'
  if [[ "$1" == first ]]; then
    said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
      -d "$(jq -c '. + {firstName: "Owner", lastName: "Example"}' <<<"$owner")" "http://127.0.0.1:$port/rest/owner/setup") || fail "n8n's owner setup got no answer"
    [[ "$said" == 200 ]] || fail "n8n didn't make its owner: it answered $said"
  fi
  n8n_sign_in() {
    curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
      -d "$(jq -c --arg p "$1" '{emailOrLdapLoginId: .email, password: $p}' <<<"$owner")" "http://127.0.0.1:$port/rest/login"
  }
  [[ "$(n8n_sign_in "$(jq -r .password <<<"$owner")")" == 200 ]] || fail "n8n's owner can't sign in"
  [[ "$(n8n_sign_in not-the-password)" == 401 ]] || fail "n8n didn't refuse a wrong password"
}
