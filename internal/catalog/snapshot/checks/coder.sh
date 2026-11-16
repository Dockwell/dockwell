# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# coder's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_coder first, once it's installed,
# and check_coder upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[coder]="2.38.2 ghcr.io/coder/coder:v2.38.2@sha256:ab17fc8e172e0f489cc1a67f513a506a49583ff692835601a614e20bb83ad34a"

check_coder() {
  local coder port built said token
  coder=$(container coder)
  port=$(value PORT)
  built=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/v2/buildinfo") || fail "Coder doesn't answer on port $port"
  [[ "$(jq -r .version <<<"$built")" == "v$version"* ]] || fail "Coder says it's $(jq -r .version <<<"$built"), want $version"
  # It starts workspaces through Docker's socket, which it reaches.
  said=$(docker exec "$coder" curl -fsS --max-time 5 --unix-socket /var/run/docker.sock http://docker/_ping) || fail "Coder can't reach Docker's socket"
  [[ "$said" == OK ]] || fail "Docker's socket answers Coder with '$said'"
  owner='{"email":"owner@example.com","password":"e2e-Owner-passw0rd-that-is-long"}'
  if [[ "$1" == first ]]; then
    said=$(curl -sS --max-time 15 -w '\n%{http_code}' -H 'Content-Type: application/json' \
      -d "$(jq -c '. + {username: "owner", name: "Owner", trial: false}' <<<"$owner")" "http://127.0.0.1:$port/api/v2/users/first") || fail "Coder's first user got no answer"
    [[ "${said##*$'\n'}" == 201 ]] || fail "Coder didn't make its first user: $said"
  fi
  # What it keeps is in PostgreSQL, so the owner signs in after an upgrade too.
  said=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' -d "$owner" "http://127.0.0.1:$port/api/v2/users/login") || fail "Coder's owner can't sign in"
  token=$(jq -r '.session_token // empty' <<<"$said")
  [[ -n "$token" ]] || fail "Coder's owner signed in with no session: $said"
  said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    -d '{"email":"owner@example.com","password":"not-the-password-at-all"}' "http://127.0.0.1:$port/api/v2/users/login") || fail "Coder's sign-in got no answer"
  [[ "$said" == 401 ]] || fail "Coder answered $said to a wrong password, want 401"
}
