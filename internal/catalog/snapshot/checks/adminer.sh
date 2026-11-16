# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# adminer's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_adminer first, once it's installed,
# and check_adminer upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[adminer]="6.1.2 adminer:6.1.2@sha256:251295c1cd51d3889f1c42ffa4864b24eeab51f0caf395cfe5bf4726c5ec9a2a"

# adminer_sign_in signs in to Adminer, as scripts/docker-e2e.sh does, to PostgreSQL at a
# server Adminer reaches, and says how it went: in, or refused, by what the page says.
adminer_sign_in() {
  local jar form token page
  jar=$(mktemp)
  form=$(curl -fsS --max-time 15 -c "$jar" -b "$jar" "http://127.0.0.1:$1/") || fail "Adminer doesn't answer on port $1"
  token=$(form_token "$form")
  [[ -n "$token" ]] || fail "Adminer's login form has no token: ${form:0:400}"
  page=$(curl -sS -L --max-time 30 -c "$jar" -b "$jar" --data-urlencode "token=$token" --data-urlencode 'auth[driver]=pgsql' --data-urlencode "auth[server]=$2" \
    --data-urlencode 'auth[username]=postgres' --data-urlencode "auth[password]=$3" --data-urlencode 'auth[db]=' "http://127.0.0.1:$1/") || fail "signing in to Adminer got no answer"
  rm -f "$jar"
  case "$page" in
  *"id='logout'"*) echo in ;;
  *"password authentication failed"*) echo refused ;;
  *) fail "Adminer neither signed in nor said why not: ${page:0:1500}" ;;
  esac
}

check_adminer() {
  local port page said
  port=$(value PORT)
  page=$(curl -fsS --max-time 15 "http://127.0.0.1:$port/") || fail "Adminer doesn't answer on port $port"
  [[ "$page" == *"auth[driver]"* && "$page" == *"$version"* ]] || fail "port $port answers something else, or not Adminer $version: ${page:0:400}"
  # A throwaway database on its project's network, which it reaches by name.
  if [[ "$1" == first ]]; then
    docker run --detach --name e2e-adminer-db --network "${project}_default" -e POSTGRES_PASSWORD=e2e-throwaway-password \
      postgres:17.11@sha256:748a18a54d3842d43bc156f0840d78029b8d349e0feca98c12b9c4ce3404a6b3 >/dev/null || fail "the throwaway database didn't start"
    for _ in $(seq 60); do
      docker exec e2e-adminer-db pg_isready -q -h 127.0.0.1 -U postgres && break
      sleep 1
    done
  fi
  said=$(adminer_sign_in "$port" e2e-adminer-db e2e-throwaway-password) || fail "signing in to Adminer failed"
  [[ "$said" == in ]] || fail "Adminer can't sign in to a database it reaches"
  said=$(adminer_sign_in "$port" e2e-adminer-db not-the-password) || fail "signing in to Adminer failed"
  [[ "$said" == refused ]] || fail "Adminer signed in with a wrong password"
  docker exec "$(container adminer)" getent hosts host.docker.internal >/dev/null || fail "Adminer doesn't reach host.docker.internal"
  # Gone before the plan takes its network away.
  if [[ "$1" == upgraded ]]; then docker rm --force e2e-adminer-db >/dev/null || fail "the throwaway database didn't go"; fi
}
