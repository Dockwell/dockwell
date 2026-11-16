# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# mariadb's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_mariadb first, once it's installed,
# and check_mariadb upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[mariadb]="12.3.2 mariadb:12.3.2@sha256:a02fe89cb597d4375812b2eac90cf9d0775d4686daa7f7cc750ebbcad7525bbc"

# What a person gives its install, as dockwell catalog install takes it.
companions[mariadb]="phpmyadmin"

# phpmyadmin_sign_in signs in to phpMyAdmin as root, as scripts/docker-e2e.sh does, and
# says how it went: in, or refused, which phpMyAdmin says as the database's own 1045.
# Anything else fails, so an error page can't pass for a refusal.
phpmyadmin_sign_in() {
  local jar form token page
  jar=$(mktemp)
  form=$(curl -fsS --max-time 15 -c "$jar" -b "$jar" "http://127.0.0.1:$1/") || fail "phpMyAdmin doesn't answer on port $1"
  [[ "$form" == *pma_username* ]] || fail "port $1 answers something else: ${form:0:400}"
  token=$(form_token "$form")
  page=$(curl -sS -L --max-time 30 -c "$jar" -b "$jar" --data-urlencode "token=$token" --data-urlencode 'pma_username=root' --data-urlencode "pma_password=$2" \
    --data-urlencode 'server=1' --data-urlencode 'route=/' "http://127.0.0.1:$1/index.php?route=/") || fail "signing in to phpMyAdmin got no answer"
  rm -f "$jar"
  case "$page" in
  *route=/logout*) echo in ;;
  *1045*) echo refused ;;
  *) fail "phpMyAdmin neither signed in nor said why not: ${page:0:400}" ;;
  esac
}

check_mariadb() {
  local db port password answer said
  db=$(container db)
  port=$(value ADMIN_PORT)
  password=$(generated db MARIADB_ROOT_PASSWORD)
  # shellcheck disable=SC2016 # the container's shell reads the password.
  maria() { docker exec "$db" sh -c 'MYSQL_PWD=$MARIADB_ROOT_PASSWORD exec mariadb -uroot -h127.0.0.1 -N -e "$1"' sh "$1"; }
  answer=$(maria 'SELECT 41 + 1') || fail "MariaDB doesn't take the password it was given"
  [[ "$answer" == 42 ]] || fail "MariaDB answered '$answer'"
  said=$(phpmyadmin_sign_in "$port" "$password") || fail "signing in to phpMyAdmin failed"
  [[ "$said" == in ]] || fail "phpMyAdmin can't sign in to MariaDB with its password"
  said=$(phpmyadmin_sign_in "$port" not-the-password) || fail "signing in to phpMyAdmin failed"
  [[ "$said" == refused ]] || fail "phpMyAdmin signed in with a wrong password"
  if [[ "$1" == first ]]; then
    maria 'CREATE DATABASE e2e_kept' || fail "MariaDB can't make a database"
    # Change password gives root a new one, which phpMyAdmin signs in with over the
    # network, where the old one is refused, and root@localhost signs in over the
    # socket, with no password, after it as it did before.
    local planned old=$password socket_before
    socket_before=$(docker exec "$db" mariadb -uroot -N -e 'SELECT 41 + 1' 2>&1) || socket_before="refused: $socket_before"
    planned=$(dw databases change-password db --project "$id" --plan-only --json) || fail "planning a new password for MariaDB failed: $planned"
    plans+=("$(jq -r .change.id <<<"$planned")")
    apply "${plans[-1]}"
    settled
    db=$(container db)
    password=$(generated db MARIADB_ROOT_PASSWORD)
    [[ "$password" != "$old" ]] || fail "MariaDB runs with the password it had"
    said=$(phpmyadmin_sign_in "$port" "$password") || fail "signing in to phpMyAdmin failed"
    [[ "$said" == in ]] || fail "phpMyAdmin can't sign in to MariaDB with its new password"
    said=$(phpmyadmin_sign_in "$port" "$old") || fail "signing in to phpMyAdmin failed"
    [[ "$said" == refused ]] || fail "phpMyAdmin still signs in to MariaDB with its old password"
    answer=$(docker exec "$db" mariadb -uroot -N -e 'SELECT 41 + 1' 2>&1) || answer="refused: $answer"
    if [[ "$socket_before" == 42 ]]; then
      [[ "$answer" == 42 ]] || fail "root@localhost lost its sign-in over the socket with a new password: $answer"
    else
      [[ "$answer" != 42 ]] || fail "root@localhost signs in over the socket with a new password, where before it said: $socket_before"
      echo "  root@localhost had no sign-in over the socket before a new password, and has none after: $socket_before"
    fi
  else
    [[ -n "$(maria "SHOW DATABASES LIKE 'e2e\\_kept'")" ]] || fail "the database made before the upgrade is gone"
  fi
}
