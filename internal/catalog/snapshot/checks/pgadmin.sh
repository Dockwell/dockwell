# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# pgadmin's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_pgadmin first, once it's installed,
# and check_pgadmin upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[pgadmin]="9.16 dpage/pgadmin4:9.16@sha256:40fa840c5bb7c8463957f1255b01283732c2d8c9396a956d180f8e6c296753b3"

# What a person gives its install, as dockwell catalog install takes it.
given[pgadmin]="ADMIN_EMAIL=owner@example.com"

# pgadmin_sign_in signs in to pgAdmin on its port, and says where it lands: its
# browser, or its login page again.
pgadmin_sign_in() {
  local jar page token landed
  jar=$(mktemp)
  page=$(curl -fsS --max-time 15 -c "$jar" -b "$jar" "http://127.0.0.1:$1/login") || fail "pgAdmin's login page doesn't answer"
  token=$(grep -o '"csrfToken": *"[^"]*"' <<<"$page" | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
  [[ -n "$token" ]] || fail "pgAdmin's login page has no token: ${page:0:400}"
  landed=$(curl -sS -L --max-time 30 -o /dev/null -w '%{url_effective}' -c "$jar" -b "$jar" --data-urlencode "csrf_token=$token" \
    --data-urlencode 'email=owner@example.com' --data-urlencode "password=$2" --data-urlencode 'language=en' --data-urlencode 'internal_button=Login' \
    "http://127.0.0.1:$1/authenticate/login") || fail "signing in to pgAdmin got no answer"
  rm -f "$jar"
  echo "$landed"
}

check_pgadmin() {
  local port password
  port=$(value PORT)
  password=$(generated pgadmin PGADMIN_DEFAULT_PASSWORD)
  [[ "$(curl -fsS --max-time 10 "http://127.0.0.1:$port/misc/ping")" == PING ]] || fail "pgAdmin doesn't answer its ping on port $port"
  local landed
  # The right one first: pgAdmin locks an account after three wrong ones in a row.
  landed=$(pgadmin_sign_in "$port" "$password") || fail "signing in to pgAdmin failed"
  [[ "$landed" == */browser/ ]] || fail "pgAdmin doesn't sign in with the password Dockwell generated: it lands on $landed"
  landed=$(pgadmin_sign_in "$port" not-the-password) || fail "signing in to pgAdmin failed"
  [[ "$landed" == */login* ]] || fail "pgAdmin didn't send a wrong password back to its login page: it lands on $landed"
  # This server, as pgAdmin reaches it, for a database it runs outside any project.
  docker exec "$(container pgadmin)" getent hosts host.docker.internal >/dev/null || fail "pgAdmin doesn't reach host.docker.internal"
}
