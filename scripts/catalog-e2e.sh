#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Tries apps from the catalog on this machine's Docker Engine, through the CLI, as a
# person would. Each app named installs from its entry as a project of its own, with
# its secrets generated and sealed: first at the version before the one it pins, when
# the script knows one, which then upgrades in one plan that backs its volumes up
# first, and keeps its data and its secrets. Each container turns healthy, runs an
# image pinned by digest, and the app answers on its port and signs in with what
# Dockwell generated, which nothing anyone can read holds. Then it goes again through
# a plan, and its volumes stay on the server for an undo, as the PRD keeps what a
# deletion removes for 7 days.
#
# scripts/docker-e2e.sh tries the first five apps more deeply, among everything else.
# CI runs this in a few jobs side by side, each naming the apps it tries, and
# internal/catalog's tests hold every entry to one of the two. An app here needs a
# check of its own, check_<id>, which says what answering means for it.
#
# It uses throwaway data directories and removes what it made, so it never touches a
# real install. It needs Docker, Go, curl, jq and openssl, and the ports the apps'
# entries suggest. SFTPGo also needs sshpass and OpenSSH's sftp and ssh-keyscan.
# With E2E_ROOT set, as in CI, Dockwell runs as root through sudo, as it does
# on a server.
#
#   scripts/catalog-e2e.sh bin/dockwell pgadmin redis
set -euo pipefail
# Anything this starts asks nothing beyond this machine (DOCKWELL_TEST, ADR 0034).
export DOCKWELL_TEST=1

usage="usage: scripts/catalog-e2e.sh path/to/dockwell ENTRY..."
for tool in docker curl go jq openssl; do
  command -v "$tool" >/dev/null || {
    echo "catalog-e2e: $tool isn't installed, and the script needs it. Install it, such as with apt-get install $tool, then run it again" >&2
    exit 1
  }
done
bin=$(realpath "${1:?$usage}")
shift
(($# > 0)) || {
  echo "$usage" >&2
  exit 1
}
addr=127.0.0.1:${E2E_PORT:-7698}
port=${addr##*:}
data=$(mktemp -d)
log=$(mktemp)
cookie=$(mktemp)
passkey=$(mktemp)
stepup=$(mktemp)
# A release other than the one built in comes from here, which Dockwell reads each time.
catalog_dir=$(mktemp -d)
chmod 755 "$catalog_dir"
# A folder of media, for an app that reads one from the server.
media_dir=$(mktemp -d)
echo kept >"$media_dir/e2e.txt"
chmod -R a+rX "$media_dir"
projects=()
plans=()
input_env=()
pid=""

as_dockwell=()
[[ -z "${E2E_ROOT:-}" ]] || as_dockwell=(sudo -n)
priv() { "${as_dockwell[@]}" "$@"; }

cleanup() {
  for p in "${projects[@]}"; do
    docker ps --all --quiet --filter "label=com.docker.compose.project=$p" | xargs --no-run-if-empty docker rm --force --volumes >/dev/null 2>&1 || true
    docker volume ls --quiet --filter "label=com.docker.compose.project=$p" | xargs --no-run-if-empty docker volume rm --force >/dev/null 2>&1 || true
    docker network ls --quiet --filter "label=com.docker.compose.project=$p" | xargs --no-run-if-empty docker network rm >/dev/null 2>&1 || true
  done
  docker ps --all --quiet --filter label=dev.dockwell.volume-tool | xargs --no-run-if-empty docker rm --force >/dev/null 2>&1 || true
  docker rm --force e2e-adminer-db >/dev/null 2>&1 || true
  [[ -z "$pid" ]] || priv kill "$pid" 2>/dev/null || true
  wait 2>/dev/null || true
  priv rm -rf "$data" "$log" "$cookie" "$passkey" "$stepup" "$catalog_dir" "$media_dir"
}
trap cleanup EXIT

# project is the Compose project of the app being tried, which its containers carry,
# and installed its Dockwell project, from its install's plan.
project=""
installed=""
fail() {
  echo "catalog-e2e: $*" >&2
  if [[ -n "$project" ]]; then
    echo "--- what runs for it" >&2
    docker ps --all --filter "label=com.docker.compose.project=$project" --format '{{.Names}} {{.Image}} {{.Status}}' >&2 || true
    for c in $(docker ps --all --quiet --filter "label=com.docker.compose.project=$project" 2>/dev/null); do
      echo "--- the log of $(docker inspect --format '{{.Name}}' "$c" 2>/dev/null)" >&2
      docker logs --tail 40 "$c" >&2 2>&1 || true
    done
  fi
  echo "--- dockwell log, its last 200 lines" >&2
  priv tail -n 200 "$log" >&2 || true
  exit 1
}
step() { echo "--- $*"; }

# Local access on the control server: the CLI finds the control plane, and makes its
# own token, from the data directory.
dw() { priv env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text "${input_env[@]}" "$bin" "$@"; }

step "Dockwell starts, and its owner claims it"
"${as_dockwell[@]}" env DOCKWELL_TEST=1 DOCKWELL_CATALOG_DIR="$catalog_dir" \
  "$bin" run --listen "$addr" --agent-listen 127.0.0.1:0 --data-dir "$data" --log-format json >>"$log" 2>&1 &
pid=$!
for _ in $(seq 100); do
  curl -fsS "http://$addr/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
[[ "$(curl -fsS "http://$addr/healthz")" == "ok" ]] || fail "/healthz did not answer ok within 10 s"
for _ in $(seq 300); do
  priv grep -q 'connected to the control plane' "$log" && break
  sleep 0.1
done
priv grep -q 'connected to the control plane' "$log" || fail "the control server's own agent didn't connect within 30 s"
claim=$(dw claim) || fail "dockwell claim failed"
link=$(grep -o 'http://localhost:[0-9]*/claim#[A-Za-z0-9_-]*' <<<"$claim" | head -1) || true
[[ -n "$link" ]] || fail "dockwell claim printed no claim link: $claim"
(cd "$(dirname "$0")/.." && go run ./scripts/claim "$link" "$cookie" "$passkey" && go build -o "$stepup" ./scripts/stepup) || fail "can't claim the install"
dw login --local >/dev/null || fail "dockwell login --local failed"

# person_answer calls the API as a signed-in person does, from the panel's origin, and
# says what it answered, its headers first, and the status last.
person_answer() {
  curl -sS --max-time 30 -D - -w '\n%{http_code}' --resolve "localhost:$port:127.0.0.1" -H "Origin: http://localhost:$port" -H 'Content-Type: application/json' \
    -H "Cookie: $(cat "$cookie")" -d "$2" "http://localhost:$port/dockwell.v1.$1"
}
change_field() { dw changes get "$1" --json | jq -r ".change.$2"; }
# settles waits for a change to finish applying: it returns 0 once it's applied, and
# 1 once it failed.
settles() {
  local state=""
  for _ in $(seq 1200); do
    state=$(change_field "$1" state) || state=""
    [[ "$state" == CHANGE_STATE_APPLIED ]] && return 0
    [[ "$state" == CHANGE_STATE_FAILED ]] && return 1
    sleep 0.5
  done
  fail "$1 is $state after 10 min, want applied"
}
# try_apply applies a planned change: with the CLI's token when the plan lets one, and
# as a person does in the panel when it destroys data or reaches the server,
# confirming with the passkey the claim made when it's as good as root. It returns 1
# when the change failed, and fails at once on anything else.
try_apply() {
  local plan fingerprint answer proof
  plan=$(dw changes get "$1" --json) || fail "can't read $1"
  if jq -e '.change.plan | (.destructive or .hostAccess) | not' <<<"$plan" >/dev/null; then
    dw changes apply "$1" --yes >/dev/null && return 0
    [[ "$(change_field "$1" state)" == CHANGE_STATE_FAILED ]] && return 1
    fail "applying $1 failed: $(dw changes get "$1" --json)"
  fi
  fingerprint=$(jq -r .change.plan.fingerprint <<<"$plan")
  answer=$(person_answer ChangeService/ApplyChange "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\"}") || fail "a person can't reach the API to apply $1"
  if [[ "${answer##*$'\n'}" == 403 ]] && grep -qi '^dockwell-error: needs_step_up' <<<"$answer"; then
    proof=$("$stepup" "http://localhost:$port" "$cookie" "$passkey" STEP_UP_ACTION_APPLY_ROOT_CHANGE "$1") || fail "can't confirm $1 with the passkey"
    answer=$(person_answer ChangeService/ApplyChange "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\",\"proof\":\"$proof\"}") || fail "a person can't reach the API to apply $1"
  fi
  [[ "${answer##*$'\n'}" == 200 ]] || fail "a person can't apply $1: $answer"
  settles "$1"
}
# apply applies a planned change, and fails at once if the change fails.
apply() { try_apply "$1" || fail "$1 failed: $(dw changes get "$1" --json)"; }
# registry_trouble says a failed change failed on a registry, and nothing about the
# image or the entry: a step of it comes down to "registry" (StepProgress.reason), a
# registry that didn't answer a pull properly; or, as CI's pulls time out too, a step
# says the server can't reach the registry, which the product leaves without a
# reason, as that may be the server's own network.
registry_trouble() {
  dw changes get "$1" --json | jq -e '.change.steps // [] | any(.reason == "registry" or ((.message // "") | contains("can'"'"'t reach the registry")))' >/dev/null
}
# apply_patiently applies $change, which the function named $1 plans, setting change.
# When it fails only as a registry didn't answer a pull properly, it says so, waits
# for the rollback Dockwell makes of it and 30 s, plans it again and applies that,
# once, as docker-e2e.sh's read_updates_patiently does for a package mirror. Anything
# else fails at once, and so does a second failure or a rollback that fails.
apply_patiently() {
  local said back
  try_apply "$change" && return 0
  registry_trouble "$change" || fail "$change failed: $(dw changes get "$change" --json)"
  said=$(dw changes get "$change" --json | jq -r '[.change.steps[] | select(.reason == "registry" or ((.message // "") | contains("can'"'"'t reach the registry"))) | "\(.reason // "no reason"): \(.message)"][0]')
  echo "catalog-e2e: $change failed on a registry, not on the entry (a step says $said). Planning it again in 30 s, once" >&2
  # The rollback Dockwell makes of it goes on record a moment after the failure.
  back=""
  for _ in $(seq 40); do
    back=$(change_field "$change" rolledBackBy)
    [[ -z "$back" || "$back" == null ]] || break
    back=""
    sleep 0.5
  done
  [[ -n "$back" ]] || fail "$change failed on a registry, and no rollback of it came within 20 s: $(dw changes get "$change" --json)"
  settles "$back" || fail "$change failed on a registry, and its rollback $back failed too: $(dw changes get "$back" --json)"
  sleep 30
  "$1"
  apply "$change"
}

# container names what a service of the app runs in.
container() {
  local out
  out=$(docker ps --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$1" --format '{{.Names}}') ||
    fail "docker ps failed"
  [[ -n "$out" && "$out" != *$'\n'* ]] || fail "Docker runs '$out' for $1, want one container"
  echo "$out"
}
# env_of reads what a container has in its environment, which is how a person's
# passkey would see a generated secret.
env_of() {
  local all
  all=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$1") || fail "docker inspect $1 failed"
  sed -n "s/^$2=//p" <<<"$all"
}
# generated reads a generated secret, and says nothing but that shows it.
generated() {
  local value
  value=$(env_of "$(container "$1")" "$2")
  [[ ${#value} -ge 32 ]] || fail "$1 got ${#value} characters for $2, want a generated value"
  echo "$value"
}
# value reads what the install's input was given.
value() {
  local installs got
  installs=$(dw catalog installs --json) || fail "catalog installs failed"
  got=$(jq -r --arg p "$installed" --arg k "$1" '.installs[] | select(.project == $p) | .pin.values[$k] // empty' <<<"$installs")
  [[ -n "$got" ]] || fail "the install $installed keeps no $1: $installs"
  echo "$got"
}
# answers waits for a URL to answer with a status under 500, and says what it answered.
answers() {
  local out=""
  for _ in $(seq 60); do
    out=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$1" 2>/dev/null) || out=""
    [[ -n "$out" && "$out" != 000 && "$out" -lt 500 ]] && return 0
    sleep 1
  done
  fail "$1 answers '$out' a minute after the install, want a page"
}
# form_token reads the token a login form carries, by its first mention, as
# scripts/docker-e2e.sh does for Adminer and phpMyAdmin.
form_token() {
  sed -n "/name=[\"']token[\"']/{s/.*name=[\"']token[\"'] value=[\"']\([^\"']*\)[\"'].*/\1/p;q;}" <<<"$1"
}

# Each app's own check, given whether this is its first version or what it upgraded to:
# it answers, signs in with what Dockwell generated, and after an upgrade finds what it
# wrote before.

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

check_redis() {
  local redis port password said bound
  redis=$(container redis)
  port=$(value PORT)
  password=$(generated redis REDISCLI_AUTH)
  bound=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$redis") || fail "docker inspect $redis failed"
  jq -e '[.[][] | .HostIp] == ["127.0.0.1"]' <<<"$bound" >/dev/null || fail "Redis is published past this server: $bound"
  # On its port, as an app on this server reaches it, in Redis's own protocol.
  said=$(exec 3<>"/dev/tcp/127.0.0.1/$port" && printf 'AUTH %s\r\nPING\r\n' "$password" >&3 && timeout 5 head -c 12 <&3) || fail "Redis doesn't answer on port $port"
  [[ "$said" == *"+OK"*"+PONG"* ]] || fail "Redis answered '${said//$'\r'/}' to its password"
  # redis-cli exits 0 or not as it likes on a refusal, so what it says decides.
  said=$(docker exec -e REDISCLI_AUTH=not-the-password "$redis" redis-cli ping 2>&1) || true
  [[ "$said" == *WRONGPASS* || "$said" == *NOAUTH* ]] || fail "Redis didn't refuse a wrong password: ${said:0:200}"
  if [[ "$1" == first ]]; then
    [[ "$(docker exec "$redis" redis-cli set e2e kept)" == OK ]] || fail "Redis doesn't keep a value"
  else
    [[ "$(docker exec "$redis" redis-cli get e2e)" == kept ]] || fail "what Redis kept didn't stay through the upgrade"
  fi
}

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

# kept holds what an app's check wrote or read before an upgrade, by the app, for the
# check after it.
declare -A kept=()

# kuma_emit sends Uptime Kuma one event over Socket.IO's HTTP polling, as its own pages
# talk to it, and prints what it acknowledged. Its sign-in and setup have no other API.
kuma_emit() {
  local base="http://127.0.0.1:$1/socket.io/?EIO=4&transport=polling" said sid
  said=$(curl -fsS --max-time 10 "$base") || fail "Uptime Kuma's socket doesn't answer"
  sid=$(sed -n 's/^0{"sid":"\([^"]*\)".*/\1/p' <<<"$said")
  [[ -n "$sid" ]] || fail "Uptime Kuma's socket opened with '${said:0:200}'"
  curl -fsS --max-time 10 -H 'Content-Type: text/plain;charset=UTF-8' --data-binary 40 "$base&sid=$sid" >/dev/null || fail "Uptime Kuma's socket didn't connect"
  curl -fsS --max-time 10 "$base&sid=$sid" >/dev/null || fail "Uptime Kuma's socket didn't connect"
  curl -fsS --max-time 10 -H 'Content-Type: text/plain;charset=UTF-8' --data-binary "420$2" "$base&sid=$sid" >/dev/null || fail "Uptime Kuma didn't take $2"
  for _ in $(seq 10); do
    said=$(curl -fsS --max-time 30 "$base&sid=$sid") || fail "Uptime Kuma's socket closed"
    said=$(tr '\036' '\n' <<<"$said" | sed -n 's/^430//p')
    [[ -n "$said" ]] && break
  done
  [[ -n "$said" ]] || fail "Uptime Kuma didn't answer $2"
  echo "$said"
}

check_uptime-kuma() {
  local port said
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/entry-page") || fail "Uptime Kuma doesn't answer on port $port"
  jq -e '.type != "setup-database"' <<<"$said" >/dev/null || fail "Uptime Kuma asks which database to use, which its entry sets: $said"
  docker exec "$(container uptime-kuma)" test -s /app/data/kuma.db || fail "Uptime Kuma keeps no database on its volume"
  if [[ "$1" == first ]]; then
    said=$(kuma_emit "$port" '["setup","owner","E2e-owner-passw0rd!"]')
    jq -e '.[0].ok == true' <<<"$said" >/dev/null || fail "Uptime Kuma didn't make its admin: $said"
  fi
  # Its admin is in its database, so they sign in after an upgrade too.
  said=$(kuma_emit "$port" '["login",{"username":"owner","password":"E2e-owner-passw0rd!","token":""}]')
  jq -e '.[0].ok == true and (.[0].token | length > 0)' <<<"$said" >/dev/null || fail "Uptime Kuma's admin can't sign in: $said"
  said=$(kuma_emit "$port" '["login",{"username":"owner","password":"not-the-password","token":""}]')
  jq -e '.[0].ok == false' <<<"$said" >/dev/null || fail "Uptime Kuma didn't refuse a wrong password: $said"
}

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

check_gitea() {
  local gitea port said banner key
  gitea=$(container gitea)
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/v1/version") || fail "Gitea doesn't answer on port $port"
  [[ "$(jq -r .version <<<"$said")" == "$version" ]] || fail "Gitea says it's $said, want $version"
  # It encrypts with the key Dockwell generated, not the one its source publishes.
  key=$(docker exec "$gitea" sed -n 's/^SECRET_KEY *= *//p' /data/gitea/conf/app.ini) || fail "Gitea has no app.ini"
  [[ ${#key} -ge 64 && "$key" == "$(env_of "$gitea" GITEA__security__SECRET_KEY)" ]] || fail "Gitea's SECRET_KEY isn't the one Dockwell generated"
  if [[ "$1" == first ]]; then
    docker exec -u git "$gitea" gitea admin user create --username owner --password E2e-owner-passw0rd --email owner@example.com --admin --must-change-password=false >/dev/null ||
      fail "Gitea can't make a user"
  fi
  # What it keeps is on its volume, so the user signs in after an upgrade too.
  said=$(curl -fsS --max-time 10 -u owner:E2e-owner-passw0rd "http://127.0.0.1:$port/api/v1/user") || fail "Gitea's user can't sign in"
  [[ "$(jq -r .login <<<"$said")" == owner ]] || fail "Gitea signed in as $said"
  said=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -u owner:not-the-password "http://127.0.0.1:$port/api/v1/user") || fail "Gitea's sign-in got no answer"
  [[ "$said" == 401 ]] || fail "Gitea answered $said to a wrong password"
  banner=$(exec 3<>"/dev/tcp/127.0.0.1/$(value SSH_PORT)" && timeout 5 head -c 7 <&3) || fail "Gitea's SSH doesn't answer"
  [[ "$banner" == SSH-2.0 ]] || fail "Gitea's SSH port answers '$banner'"
}

# vaultwarden_sign_in signs in to Vaultwarden as Bitwarden's apps do, with what stands
# for the master password's hash, and prints the status and what it answered.
vaultwarden_sign_in() {
  curl -sS --max-time 15 -w '\n%{http_code}' --data-urlencode grant_type=password --data-urlencode username=owner@example.com \
    --data-urlencode "password=$2" --data-urlencode 'scope=api offline_access' --data-urlencode client_id=web --data-urlencode deviceType=10 \
    --data-urlencode deviceIdentifier=0d5b4c2e-7c3e-4b6a-9f7a-1e2d3c4b5a69 --data-urlencode deviceName=e2e "http://127.0.0.1:$1/identity/connect/token"
}

check_vaultwarden() {
  local port said token hash=ZTJlLW1hc3Rlci1wYXNzd29yZC1oYXNoLXN0YW5kLWlu
  port=$(value PORT)
  curl -fsS --max-time 10 -o /dev/null "http://127.0.0.1:$port/alive" || fail "Vaultwarden doesn't answer on port $port"
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/config") || fail "Vaultwarden's API doesn't answer"
  [[ "$(jq -r .server.name <<<"$said")" == Vaultwarden ]] || fail "port $port answers something else: ${said:0:300}"
  if [[ "$1" == first ]]; then
    # With no mail server, it gives the token a verification email would carry.
    token=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' -H 'Accept: application/json' \
      -d '{"email":"owner@example.com","name":"Owner"}' "http://127.0.0.1:$port/identity/accounts/register/send-verification-email") ||
      fail "Vaultwarden doesn't take a new account"
    token=$(jq -r . <<<"$token")
    said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg t "$token" --arg h "$hash" '{email: "owner@example.com", masterPasswordHash: $h, emailVerificationToken: $t, kdf: 0, kdfIterations: 600000,
        userSymmetricKey: "2.AAAAAAAAAAAAAAAAAAAAAA==|AAAAAAAAAAAAAAAAAAAAAA==|AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}')" \
      "http://127.0.0.1:$port/identity/accounts/register/finish") || fail "Vaultwarden's registration got no answer"
    [[ "$said" == 200 ]] || fail "Vaultwarden didn't make the account: it answered $said"
  fi
  # The account is in its database, so it signs in after an upgrade too.
  said=$(vaultwarden_sign_in "$port" "$hash") || fail "Vaultwarden's sign-in got no answer"
  [[ "${said##*$'\n'}" == 200 && -n "$(jq -r '.access_token // empty' <<<"${said%$'\n'*}")" ]] || fail "Vaultwarden's account can't sign in: ${said:0:300}"
  said=$(vaultwarden_sign_in "$port" bm90LXRoZS1wYXNzd29yZA==) || fail "Vaultwarden's sign-in got no answer"
  [[ "${said##*$'\n'}" == 400 && "$said" == *"Username or password is incorrect"* && "$said" != *access_token* ]] ||
    fail "Vaultwarden didn't refuse a wrong password: ${said:0:300}"
}

check_nextcloud() {
  local port password said
  port=$(value PORT)
  password=$(generated nextcloud NEXTCLOUD_ADMIN_PASSWORD)
  # As localhost, as an SSH tunnel reaches it: it answers only at what it trusts.
  nc_curl() { curl -H 'Host: localhost' "$@"; }
  said=$(nc_curl -fsS --max-time 15 "http://127.0.0.1:$port/status.php") || fail "Nextcloud doesn't answer on port $port"
  jq -e --arg v "$version" '.installed == true and .versionstring == $v and .maintenance == false' <<<"$said" >/dev/null ||
    fail "Nextcloud says $said, want $version installed"
  nextcloud_user() {
    nc_curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -u "admin:$1" -H 'OCS-APIRequest: true' "http://127.0.0.1:$port/ocs/v2.php/cloud/user?format=json"
  }
  said=$(nextcloud_user "$password") || fail "Nextcloud's sign-in got no answer"
  [[ "$said" == 200 ]] || fail "Nextcloud doesn't sign in with the password Dockwell generated: it answered $said"
  if [[ "$1" == first ]]; then
    nc_curl -fsS --max-time 15 -u "admin:$password" -X PUT --data-binary kept "http://127.0.0.1:$port/remote.php/dav/files/admin/e2e.txt" ||
      fail "Nextcloud doesn't take a file"
  fi
  said=$(nc_curl -fsS --max-time 15 -u "admin:$password" "http://127.0.0.1:$port/remote.php/dav/files/admin/e2e.txt") || fail "Nextcloud doesn't give back the file it took"
  [[ "$said" == kept ]] || fail "Nextcloud gave back '$said'"
  docker exec -u www-data "$(container nextcloud)" php occ config:system:get trusted_domains | grep -qx cloud.example.com ||
    fail "Nextcloud doesn't answer at the domain it was given"
  # Last, as a wrong password slows what comes after it from the same address.
  said=$(nextcloud_user not-the-password) || fail "Nextcloud's sign-in got no answer"
  [[ "$said" == 401 ]] || fail "Nextcloud answered $said to a wrong password"
}

check_rustfs() {
  local port console key secret said
  port=$(value S3_PORT)
  console=$(value CONSOLE_PORT)
  key=$(value ACCESS_KEY)
  secret=$(generated rustfs RUSTFS_SECRET_KEY)
  curl -fsS --max-time 10 -o /dev/null "http://127.0.0.1:$console/rustfs/console/health" || fail "RustFS's console doesn't answer on port $console"
  # Named outright, as S3 asks for it and some versions of curl don't send it.
  s3() { curl -fsS --max-time 15 --aws-sigv4 aws:amz:us-east-1:s3 --user "$key:$1" -H 'x-amz-content-sha256: UNSIGNED-PAYLOAD' "${@:2}"; }
  if [[ "$1" == first ]]; then
    s3 "$secret" -X PUT "http://127.0.0.1:$port/e2e-kept" >/dev/null || fail "RustFS doesn't make a bucket with the key Dockwell generated"
    s3 "$secret" -X PUT --data-binary kept "http://127.0.0.1:$port/e2e-kept/marker" >/dev/null || fail "RustFS doesn't take an object"
  fi
  [[ "$(s3 "$secret" "http://127.0.0.1:$port/e2e-kept/marker")" == kept ]] || fail "RustFS doesn't give back the object it took"
  said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' --aws-sigv4 aws:amz:us-east-1:s3 --user "$key:not-the-secret-key-at-all" \
    -H 'x-amz-content-sha256: UNSIGNED-PAYLOAD' "http://127.0.0.1:$port/e2e-kept/marker") || fail "RustFS got no answer"
  [[ "$said" == 403 ]] || fail "RustFS answered $said to a wrong secret key, want 403"
}

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

check_plausible() {
  local plausible port said db events migrations tables jar token landed
  plausible=$(container plausible)
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/system/health/ready") || fail "Plausible isn't ready on port $port"
  jq -e . <<<"$said" >/dev/null || fail "Plausible's readiness says what isn't JSON: ${said:0:300}"
  db=$(container db)
  events=$(container events)
  migrations=$(docker exec "$db" psql -X -U plausible -d plausible -tAc 'SELECT count(*) FROM schema_migrations') || fail "Plausible's PostgreSQL has no migrations"
  tables=$(docker exec "$events" clickhouse-client -q "SELECT count() FROM system.tables WHERE database = 'plausible_events_db'") || fail "Plausible's ClickHouse doesn't answer"
  ((migrations > 0 && tables > 0)) || fail "Plausible has $migrations migrations in PostgreSQL and $tables tables in ClickHouse"
  said=$(docker exec "$events" clickhouse-client -q "SELECT count() FROM system.tables WHERE database = 'system' AND name = 'trace_log'") || fail "Plausible's ClickHouse doesn't answer"
  [[ "$said" == 0 ]] || fail "ClickHouse keeps a trace log, which Plausible's settings turn off"
  if [[ "$1" == first ]]; then
    # On first launch it leads to registration.
    said=$(curl -sS --max-time 10 -o /dev/null -w '%{redirect_url}' "http://127.0.0.1:$port/") || fail "Plausible doesn't answer on port $port"
    [[ "$said" == */register ]] || fail "Plausible's first page leads to '$said', want its registration"
    # Its registration is a live page, which curl can't drive, so its own release makes the account.
    docker exec "$plausible" /app/bin/plausible rpc \
      'Plausible.Repo.insert!(Plausible.Auth.User.new(%{"name" => "Owner", "email" => "owner@example.com", "password" => "Zq7-Tm2!vLx9-Pw4rK", "password_confirmation" => "Zq7-Tm2!vLx9-Pw4rK"}))' >/dev/null ||
      fail "Plausible's release can't make an account"
  fi
  # The account is in PostgreSQL, so it signs in after an upgrade too.
  plausible_sign_in() {
    jar=$(mktemp)
    said=$(curl -fsS --max-time 15 -c "$jar" -b "$jar" "http://127.0.0.1:$port/login") || fail "Plausible's login page doesn't answer"
    token=$(sed -n 's/.*name="_csrf_token"[^>]*value="\([^"]*\)".*/\1/p' <<<"$said" | head -1)
    [[ -n "$token" ]] || fail "Plausible's login page has no token: ${said:0:300}"
    landed=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code} %{redirect_url}' -c "$jar" -b "$jar" --data-urlencode "_csrf_token=$token" \
      --data-urlencode email=owner@example.com --data-urlencode "password=$1" "http://127.0.0.1:$port/login") || fail "signing in to Plausible got no answer"
    rm -f "$jar"
    echo "$landed"
  }
  landed=$(plausible_sign_in 'Zq7-Tm2!vLx9-Pw4rK') || fail "signing in to Plausible failed"
  [[ "$landed" == 302\ */sites* ]] || fail "Plausible's account can't sign in: it answered $landed"
  landed=$(plausible_sign_in not-the-password) || fail "signing in to Plausible failed"
  [[ "$landed" == "200 " ]] || fail "Plausible didn't refuse a wrong password: it answered $landed"
  if [[ "$1" == first ]]; then kept[plausible]=$tables; fi
  ((tables >= kept[plausible])) || fail "Plausible's ClickHouse had ${kept[plausible]} tables before the upgrade, and has $tables"
}

check_ghost() {
  local ghost port said reported
  ghost=$(container ghost)
  port=$(value PORT)
  # It's given an https address, as behind a domain, so it answers what the proxy in front
  # sends, and sends anything else there. Its health check asks as that proxy does.
  said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code} %{redirect_url}' "http://127.0.0.1:$port/ghost/api/admin/site/") || fail "Ghost doesn't answer on port $port"
  [[ "$said" == 30?\ https://blog.example.com/ghost/* ]] || fail "Ghost answers '$said' to plain http, want it sent to https://blog.example.com"
  said=$(ghost_get "$port" /ghost/api/admin/site/) || fail "Ghost doesn't answer on port $port behind an https proxy"
  # It says its version without the patch, and its image says the whole of it.
  reported=$(jq -r '.site.version // empty' <<<"$said")
  [[ -n "$reported" && "$reported" == "${version%.*}" && "$(env_of "$ghost" GHOST_VERSION)" == "$version" ]] ||
    fail "Ghost says it's '$reported' and its image '$(env_of "$ghost" GHOST_VERSION)', want $version"
  [[ "$(env_of "$ghost" url)" == https://blog.example.com ]] || fail "Ghost's url is '$(env_of "$ghost" url)', want the address it was given"
  if [[ "$1" == first ]]; then
    # Ghost refuses a password with a common word in it, such as passw0rd, so this has none.
    said=$(curl -sS --max-time 30 -w '\n%{http_code}' -H 'X-Forwarded-Proto: https' -H 'Content-Type: application/json' -H 'Origin: https://blog.example.com' \
      -d '{"setup":[{"name":"Owner","email":"owner@example.com","password":"Kx7-Vq2!mRt9-Lw4zB","blogTitle":"E2E kept"}]}' \
      "http://127.0.0.1:$port/ghost/api/admin/authentication/setup/") || fail "Ghost's setup got no answer"
    [[ "${said##*$'\n'}" == 201 ]] || fail "Ghost didn't make its owner: it answered ${said##*$'\n'}, saying ${said%$'\n'*}"
  fi
  # What it keeps is in MySQL, so the site keeps its title through an upgrade.
  said=$(ghost_get "$port" /ghost/api/admin/site/) || fail "Ghost doesn't answer on port $port behind an https proxy"
  [[ "$(jq -r .site.title <<<"$said")" == "E2E kept" ]] || fail "Ghost's site reads $said"
}

# ghost_get asks Ghost as the https proxy in front of it does.
ghost_get() {
  curl -fsS --max-time 15 -H 'X-Forwarded-Proto: https' "http://127.0.0.1:$1$2"
}

# wordpress_sign_in signs in to WordPress, and says how it answered and where it sends
# you: its dashboard, or nowhere, as its login page says why on the page itself.
wordpress_sign_in() {
  local jar landed
  jar=$(mktemp)
  curl -fsS --max-time 15 -o /dev/null -c "$jar" "http://127.0.0.1:$1/wp-login.php" || fail "WordPress's login page doesn't answer"
  landed=$(curl -sS --max-time 30 -o /dev/null -w '%{http_code} %{redirect_url}' -b "$jar" -c "$jar" --data-urlencode log=owner --data-urlencode "pwd=$2" \
    --data-urlencode 'wp-submit=Log In' --data-urlencode testcookie=1 --data-urlencode "redirect_to=http://127.0.0.1:$1/wp-admin/" \
    "http://127.0.0.1:$1/wp-login.php") || fail "signing in to WordPress got no answer"
  rm -f "$jar"
  echo "$landed"
}

check_wordpress() {
  local port said
  port=$(value PORT)
  if [[ "$1" == first ]]; then
    said=$(curl -fsS --max-time 60 --data-urlencode 'weblog_title=E2E kept' --data-urlencode user_name=owner --data-urlencode admin_password=E2e-owner-passw0rd-long \
      --data-urlencode admin_password2=E2e-owner-passw0rd-long --data-urlencode admin_email=owner@example.com --data-urlencode blog_public=0 \
      --data-urlencode language= --data-urlencode 'Submit=Install WordPress' "http://127.0.0.1:$port/wp-admin/install.php?step=2") || fail "WordPress's install doesn't answer"
    [[ "$said" == *wp-login.php* ]] || fail "WordPress didn't install: ${said:0:600}"
  fi
  said=$(wordpress_sign_in "$port" E2e-owner-passw0rd-long) || fail "signing in to WordPress failed"
  [[ "$said" == 302\ */wp-admin/ ]] || fail "WordPress's admin can't sign in: it answered $said"
  said=$(wordpress_sign_in "$port" not-the-password) || fail "signing in to WordPress failed"
  [[ "$said" == "200 " ]] || fail "WordPress didn't refuse a wrong password: it answered $said"
  said=$(curl -fsS --max-time 15 "http://127.0.0.1:$port/?rest_route=/") || fail "WordPress's API doesn't answer"
  [[ "$(jq -r .name <<<"$said")" == "E2E kept" ]] || fail "WordPress's site reads ${said:0:300}"
}

check_memos() {
  local port said token
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/v1/instance/profile") || fail "Memos doesn't answer on port $port"
  [[ "$(jq -r .version <<<"$said")" == "$version" ]] || fail "Memos says $said, want $version"
  if [[ "$1" == first ]]; then
    said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
      -d '{"username":"owner","password":"E2e-owner-passw0rd"}' "http://127.0.0.1:$port/api/v1/users") || fail "Memos's first user got no answer"
    [[ "$said" == 200 ]] || fail "Memos didn't make its first user: it answered $said"
  fi
  memos_sign_in() {
    curl -sS --max-time 15 -H 'Content-Type: application/json' \
      -d "{\"passwordCredentials\":{\"username\":\"owner\",\"password\":\"$1\"}}" "http://127.0.0.1:$port/api/v1/auth/signin"
  }
  # The right password gives a token, and the token says who it signed in.
  said=$(memos_sign_in E2e-owner-passw0rd) || fail "Memos's sign-in got no answer"
  token=$(jq -r '.accessToken // empty' <<<"$said" 2>/dev/null)
  [[ -n "$token" ]] || fail "Memos's admin can't sign in: ${said:0:300}"
  said=$(curl -sS --max-time 15 -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/v1/auth/me") || fail "Memos's current user got no answer"
  [[ "$(jq -r '.user.username // empty' <<<"$said" 2>/dev/null)" == owner ]] || fail "Memos's token doesn't sign in as owner: ${said:0:300}"
  # Memos 0.30 and 0.31 refuse a wrong password as InvalidArgument, which its
  # gateway answers with 400, saying "unmatched username and password", and no token.
  said=$(memos_sign_in not-the-password) || fail "Memos's sign-in got no answer"
  jq -e '(.accessToken // "") == "" and (.message // "" | contains("unmatched username and password"))' <<<"$said" >/dev/null 2>&1 ||
    fail "Memos didn't refuse a wrong password: ${said:0:300}"
}

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

# jellyfin_sign_in signs in to Jellyfin as its apps do, and says the status.
jellyfin_sign_in() {
  curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    -H 'Authorization: MediaBrowser Client="e2e", Device="e2e", DeviceId="e2e-catalog", Version="1.0"' \
    -d "{\"Username\":\"owner\",\"Pw\":\"$2\"}" "http://127.0.0.1:$1/Users/AuthenticateByName"
}

check_jellyfin() {
  local jellyfin port said
  jellyfin=$(container jellyfin)
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/System/Info/Public") || fail "Jellyfin doesn't answer on port $port"
  [[ "$(jq -r .Version <<<"$said")" == "$version"* ]] || fail "Jellyfin says $said, want $version"
  docker exec "$jellyfin" test -r /media/e2e.txt || fail "Jellyfin can't read the media folder it was given"
  if docker exec "$jellyfin" touch /media/written 2>/dev/null; then fail "Jellyfin can write to the media folder, which it reads alone"; fi
  if [[ "$1" == first ]]; then
    # Its setup page, as the first person to open it goes through it.
    jf() { curl -fsS --max-time 15 -o /dev/null -H 'Content-Type: application/json' "$@"; }
    jf -d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' "http://127.0.0.1:$port/Startup/Configuration" || fail "Jellyfin's setup doesn't take its settings"
    curl -fsS --max-time 15 -o /dev/null "http://127.0.0.1:$port/Startup/User" || fail "Jellyfin's setup has no first user"
    jf -d '{"Name":"owner","Password":"E2e-owner-passw0rd"}' "http://127.0.0.1:$port/Startup/User" || fail "Jellyfin's setup doesn't take its first user"
    jf -X POST "http://127.0.0.1:$port/Startup/Complete" || fail "Jellyfin's setup doesn't finish"
  fi
  # Its account is on its volume, so it signs in after an upgrade too.
  said=$(jellyfin_sign_in "$port" E2e-owner-passw0rd) || fail "Jellyfin's sign-in got no answer"
  [[ "$said" == 200 ]] || fail "Jellyfin's account can't sign in: it answered $said"
  said=$(jellyfin_sign_in "$port" not-the-password) || fail "Jellyfin's sign-in got no answer"
  [[ "$said" == 401 ]] || fail "Jellyfin answered $said to a wrong password, want 401"
}

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

check_gotify() {
  local port base password said app token
  port=$(value PORT)
  base="http://127.0.0.1:$port"
  password=$(generated gotify GOTIFY_DEFAULTUSER_PASS)
  said=$(curl -fsS --max-time 10 "$base/health") || fail "Gotify's database health doesn't answer"
  jq -e '.health == "green" and .database == "green"' <<<"$said" >/dev/null || fail "Gotify's database isn't healthy"
  said=$(curl -sS --max-time 10 -u admin:not-the-password -o /dev/null -w '%{http_code}' "$base/current/user") || fail "Gotify's sign-in doesn't answer"
  [[ "$said" == 401 ]] || fail "Gotify answers $said to a wrong password, want 401"
  said=$(curl -fsS --max-time 10 -u "admin:$password" "$base/current/user") || fail "Gotify doesn't take its generated password"
  jq -e '.name == "admin" and .admin' <<<"$said" >/dev/null || fail "Gotify doesn't sign in as the admin"
  if [[ "$1" == first ]]; then
    said=$(curl -fsS --max-time 10 -u "admin:$password" -H 'Content-Type: application/json' -d '{"name":"Catalog e2e"}' "$base/application") || fail "Gotify doesn't create an application"
    app=$(jq -er '.id' <<<"$said") || fail "Gotify created no application ID"
    token=$(jq -er '.token | select(length > 0)' <<<"$said") || fail "Gotify created no application token"
    kept[gotify-app]=$app
    kept[gotify-token]=$token
    curl -fsS --max-time 10 -H "X-Gotify-Key: $token" -H 'Content-Type: application/json' \
      -d '{"title":"Catalog upgrade","message":"kept-through-upgrade","priority":1}' "$base/message" >/dev/null || fail "Gotify doesn't take a notification"
  else
    said=$(curl -fsS --max-time 10 -u "admin:$password" "$base/application") || fail "Gotify can't read its applications after upgrade"
    # The list omits private tokens. Using the original token proves it stayed;
    # a different message keeps that proof apart from the one already stored.
    jq -e --argjson id "${kept[gotify-app]}" 'any(.id == $id and .name == "Catalog e2e")' <<<"$said" >/dev/null || fail "Gotify lost its application through upgrade"
    said=$(curl -fsS --max-time 10 -H "X-Gotify-Key: ${kept[gotify-token]}" -H 'Content-Type: application/json' \
      -d '{"title":"Catalog upgrade","message":"sent-after-upgrade","priority":1}' "$base/message") || fail "Gotify no longer accepts its original application token"
    jq -e --argjson id "${kept[gotify-app]}" '.appid == $id and .message == "sent-after-upgrade"' <<<"$said" >/dev/null || fail "Gotify didn't send through the original application"
  fi
  said=$(curl -fsS --max-time 10 -u "admin:$password" "$base/message") || fail "Gotify can't read its notifications"
  jq -e --argjson id "${kept[gotify-app]}" '.messages | any(.appid == $id and .message == "kept-through-upgrade")' <<<"$said" >/dev/null || fail "Gotify lost its notification through upgrade"
  if [[ "$1" == upgraded ]]; then
    jq -e --argjson id "${kept[gotify-app]}" '.messages | any(.appid == $id and .message == "sent-after-upgrade")' <<<"$said" >/dev/null || fail "Gotify didn't keep its new notification"
  fi
}

# A real browser login proves the generated password works. The API is reached with
# the same session and CSRF cookie, so no token needs to be read from its database.
check_linkding() {
  local port base password jar page csrf status said wrong
  port=$(value PORT)
  base="http://127.0.0.1:$port"
  password=$(generated linkding LD_SUPERUSER_PASSWORD)
  jar=$(mktemp)
  page=$(curl -fsS --max-time 15 -c "$jar" "$base/login/") || fail "linkding's login page doesn't answer"
  csrf=$(sed -n 's/.*name="csrfmiddlewaretoken" value="\([^"]*\)".*/\1/p' <<<"$page" | head -n 1)
  [[ -n "$csrf" ]] || fail "linkding's login has no CSRF token"
  # A wrong password gets the login form back and no session. linkding answers it
  # with 401, as its 1.46.2 image did in CI, where Django's own view says 200. What
  # counts is that it's the form again, and that the API still refuses the session.
  wrong=$(mktemp)
  status=$(curl -sS --max-time 15 -b "$jar" -c "$jar" -o "$wrong" -w '%{http_code}' \
    --data-urlencode "csrfmiddlewaretoken=$csrf" --data-urlencode username=admin --data-urlencode password=not-the-password "$base/login/") || fail "linkding's sign-in doesn't answer"
  [[ "$status" == 401 || "$status" == 200 ]] || fail "linkding answers $status to a wrong password, want its login form again, with 401 or 200"
  grep -q 'name="csrfmiddlewaretoken"' "$wrong" || fail "linkding's answer to a wrong password isn't its login form"
  rm -f "$wrong"
  status=$(curl -sS --max-time 10 -b "$jar" -o /dev/null -w '%{http_code}' "$base/api/bookmarks/") || fail "linkding's API doesn't answer"
  [[ "$status" == 401 || "$status" == 403 ]] || fail "linkding's API answers $status after a wrong password, want it refused"
  status=$(curl -sS --max-time 15 -b "$jar" -c "$jar" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrfmiddlewaretoken=$csrf" --data-urlencode username=admin --data-urlencode "password=$password" "$base/login/") || fail "linkding's sign-in doesn't answer"
  [[ "$status" == 302 ]] || fail "linkding answers $status to its generated password, want a signed-in redirect"
  # linkding names its CSRF cookie ld_csrftoken, and rotates it as you sign in.
  csrf=$(awk '$6 == "ld_csrftoken" { print $7 }' "$jar" | tail -n 1)
  [[ -n "$csrf" ]] || fail "linkding's signed-in session has no ld_csrftoken cookie"
  if [[ "$1" == first ]]; then
    # This user's test fixture doesn't fetch icons, previews or archives from the
    # Internet. The bookmark itself goes through the signed-in public API.
    docker exec "$(container linkding)" python manage.py shell -c \
      'from django.contrib.auth import get_user_model; from bookmarks.models import UserProfile; p=get_user_model().objects.get(username="admin").profile; p.web_archive_integration=UserProfile.WEB_ARCHIVE_INTEGRATION_DISABLED; p.enable_favicons=False; p.enable_preview_images=False; p.enable_automatic_html_snapshots=False; p.save()' >/dev/null || fail "can't turn off external bookmark loading for this fixture"
    said=$(curl -fsS --max-time 15 -b "$jar" -H "X-CSRFToken: $csrf" -H 'Content-Type: application/json' \
      -d '{"url":"https://example.invalid/catalog-e2e","title":"Kept through upgrade","description":"catalog-e2e"}' "$base/api/bookmarks/?disable_scraping&disable_html_snapshot") || fail "linkding doesn't save a bookmark"
    kept[linkding-bookmark]=$(jq -er .id <<<"$said") || fail "linkding saved no bookmark ID"
  fi
  said=$(curl -fsS --max-time 10 -b "$jar" "$base/api/bookmarks/${kept[linkding-bookmark]}/") || fail "linkding can't read its saved bookmark"
  jq -e '.url == "https://example.invalid/catalog-e2e" and .title == "Kept through upgrade" and .description == "catalog-e2e"' <<<"$said" >/dev/null || fail "linkding's bookmark didn't stay through upgrade"
  rm -f "$jar"
}

check_new-api() {
  local port base password jar said user
  port=$(value PORT)
  base="http://127.0.0.1:$port"
  generated new-api SESSION_SECRET >/dev/null
  if [[ "$1" == first ]]; then
    kept[new-api-password]="$(openssl rand -hex 24)"
    said=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg p "${kept[new-api-password]}" '{username:"owner",password:$p,confirmPassword:$p,SelfUseModeEnabled:true,DemoSiteEnabled:false}')" "$base/api/setup") || fail "new-api's setup doesn't answer"
    jq -e '.success' <<<"$said" >/dev/null || fail "new-api couldn't make its owner"
  fi
  password=${kept[new-api-password]}
  jar=$(mktemp)
  said=$(curl -fsS --max-time 15 -c "$jar" -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg p "$password" '{username:"owner",password:$p}')" "$base/api/user/login") || fail "new-api's sign-in doesn't answer"
  jq -e '.success and .data.username == "owner"' <<<"$said" >/dev/null || fail "new-api doesn't sign in as its owner"
  user=$(jq -er .data.id <<<"$said") || fail "new-api's owner has no ID"
  if [[ "$1" == first ]]; then
    said=$(curl -fsS --max-time 15 -b "$jar" -H "New-Api-User: $user" -H 'Content-Type: application/json' \
      -d '{"name":"kept-through-upgrade","expired_time":-1,"unlimited_quota":true}' "$base/api/token/") || fail "new-api can't make an API token"
    jq -e '.success' <<<"$said" >/dev/null || fail "new-api didn't make an API token"
  fi
  said=$(curl -fsS --max-time 15 -b "$jar" -H "New-Api-User: $user" "$base/api/token/") || fail "new-api can't read its tokens"
  jq -e '.success and (.data.items | any(.name == "kept-through-upgrade"))' <<<"$said" >/dev/null || fail "new-api lost its API token through upgrade"
  said=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' -d '{"username":"owner","password":"not-the-password"}' "$base/api/user/login") || fail "new-api's sign-in doesn't answer"
  jq -e '.success == false' <<<"$said" >/dev/null || fail "new-api takes a wrong password"
  said=$(curl -fsS --max-time 10 "$base/api/setup") || fail "new-api's setup state doesn't answer"
  jq -e '.success and .data.status' <<<"$said" >/dev/null || fail "new-api forgot that setup was finished"
  said=$(docker logs "$(container new-api)" 2>&1) || fail "new-api's log can't be read"
  [[ "$said" != *"$password"* ]] || fail "new-api writes its setup password in its log"
  rm -f "$jar"
}

check_openlist() {
  local port base said token password
  port=$(value PORT)
  base="http://127.0.0.1:$port"
  [[ "$(curl -fsS --max-time 10 "$base/ping")" == pong ]] || fail "OpenList's ping doesn't answer"
  if [[ "$1" == first ]]; then
    kept[openlist-password]="$(openssl rand -hex 24)"
    said=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg p "${kept[openlist-password]}" '{username:"owner",password:$p,site_title:"Kept through upgrade"}')" "$base/api/public/init/setup") || fail "OpenList's setup doesn't answer"
    jq -e '.code == 200' <<<"$said" >/dev/null || fail "OpenList couldn't make its owner"
  fi
  password=${kept[openlist-password]}
  said=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' -d "$(jq -nc --arg p "$password" '{username:"owner",password:$p}')" "$base/api/auth/login") || fail "OpenList's sign-in doesn't answer"
  token=$(jq -er '.data.token | select(length > 0)' <<<"$said") || fail "OpenList doesn't sign in as its owner"
  said=$(curl -fsS --max-time 15 -H "Authorization: $token" "$base/api/admin/setting/get?key=site_title") || fail "OpenList can't read its settings"
  jq -e '.code == 200 and .data.value == "Kept through upgrade"' <<<"$said" >/dev/null || fail "OpenList lost its title through upgrade"
  said=$(curl -sS --max-time 15 -H 'Content-Type: application/json' -d '{"username":"owner","password":"not-the-password"}' "$base/api/auth/login") || fail "OpenList's sign-in doesn't answer"
  jq -e '.code == 401 and (.data.token // "") == ""' <<<"$said" >/dev/null || fail "OpenList takes a wrong password"
  said=$(curl -fsS --max-time 10 "$base/api/public/init_status") || fail "OpenList's setup state doesn't answer"
  jq -e '.code == 200 and .data.initialized' <<<"$said" >/dev/null || fail "OpenList forgot that setup was finished"
  said=$(docker logs "$(container openlist)" 2>&1) || fail "OpenList's log can't be read"
  [[ "$said" != *"$password"* ]] || fail "OpenList writes its setup password in its log"
}

check_sftpgo() {
  command -v sshpass >/dev/null || fail "the SFTPGo check needs sshpass, which its CI group installs"
  local port sftp_port base password said token transfer_dir status
  port=$(value PORT)
  sftp_port=$(value SFTP_PORT)
  base="http://127.0.0.1:$port"
  password=$(generated sftpgo SFTPGO_DEFAULT_ADMIN_PASSWORD)
  said=$(curl -fsS --max-time 15 -u "admin:$password" "$base/api/v2/token") || fail "SFTPGo doesn't take its generated admin password"
  token=$(jq -er '.access_token | select(length > 0)' <<<"$said") || fail "SFTPGo's admin sign-in made no token"
  status=$(curl -sS --max-time 15 -u admin:not-the-password -o /dev/null -w '%{http_code}' "$base/api/v2/token") || fail "SFTPGo's sign-in doesn't answer"
  [[ "$status" == 401 ]] || fail "SFTPGo answers $status to a wrong admin password, want 401"
  transfer_dir=$(mktemp -d)
  if [[ "$1" == first ]]; then
    kept[sftpgo-password]="$(openssl rand -hex 24)"
    said=$(curl -fsS --max-time 15 -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg p "${kept[sftpgo-password]}" '{username:"transfer",password:$p,status:1,home_dir:"/srv/sftpgo/data/transfer",permissions:{"/":["*"]}}')" "$base/api/v2/users") || fail "SFTPGo doesn't make a transfer user"
    jq -e '.username == "transfer"' <<<"$said" >/dev/null || fail "SFTPGo made no transfer user"
    kept[sftpgo-hostkey]=$(ssh-keyscan -p "$sftp_port" 127.0.0.1 2>/dev/null) || fail "SFTPGo's SSH listener doesn't answer"
    [[ -n "${kept[sftpgo-hostkey]}" ]] || fail "SFTPGo offered no SSH host key"
    printf '%s\n' 'kept-through-upgrade' >"$transfer_dir/marker"
    printf '%s\n' "${kept[sftpgo-hostkey]}" >"$transfer_dir/known_hosts"
    # -b adds BatchMode=yes, and OpenSSH takes the first value, so the override goes first.
    SSHPASS=${kept[sftpgo-password]} sshpass -e sftp -q -oBatchMode=no -b - -P "$sftp_port" \
      -oStrictHostKeyChecking=yes -oConnectTimeout=10 -oUserKnownHostsFile="$transfer_dir/known_hosts" transfer@127.0.0.1 <<EOF || fail "SFTPGo doesn't accept an SFTP upload"
put $transfer_dir/marker marker
EOF
  fi
  # Keep trusting the original host key after the container and image change too.
  printf '%s\n' "${kept[sftpgo-hostkey]}" >"$transfer_dir/known_hosts"
  SSHPASS=${kept[sftpgo-password]} sshpass -e sftp -q -oBatchMode=no -b - -P "$sftp_port" \
    -oStrictHostKeyChecking=yes -oConnectTimeout=10 -oUserKnownHostsFile="$transfer_dir/known_hosts" transfer@127.0.0.1 <<EOF || fail "SFTPGo doesn't return the uploaded file with its original host key"
get marker $transfer_dir/read-back
EOF
  [[ "$(cat "$transfer_dir/read-back")" == kept-through-upgrade ]] || fail "SFTPGo lost its uploaded file through upgrade"
  rm -rf "$transfer_dir"
}

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

check_openclaw() {
  local port c token said
  port=$(value PORT)
  c=$(container openclaw)
  token=$(generated openclaw OPENCLAW_GATEWAY_TOKEN)
  answers "http://127.0.0.1:$port/"
  # Use a fresh client identity for the negative check, so a retained paired
  # device's token can't authenticate it in place of the deliberately wrong one.
  if said=$(timeout 35 docker exec -e OPENCLAW_STATE_DIR=/tmp/catalog-wrong-token -e OPENCLAW_GATEWAY_TOKEN=not-the-token \
    "$c" node /app/dist/index.js gateway call health --url ws://127.0.0.1:18789 --token not-the-token --timeout 10000 --json 2>&1); then
    fail "OpenClaw accepts a wrong gateway token"
  fi
  [[ "$said" == *unauthorized* || "$said" == *"token mismatch"* || "$said" == *"AUTH_TOKEN_MISMATCH"* ]] || fail "OpenClaw's wrong-token check failed without an auth refusal: ${said:0:300}"
  said=$(timeout 35 docker exec "$c" node /app/dist/index.js gateway call health --url ws://127.0.0.1:18789 --token "$token" --timeout 10000 --json) || fail "OpenClaw doesn't take its generated gateway token"
  jq -e '.ok' <<<"$said" >/dev/null || fail "OpenClaw's authenticated health isn't ready"
  if [[ "$1" == first ]]; then
    # Agent state is files in its workspace. No paid model or external channel
    # is needed to prove that a real retained workspace survives replacement.
    docker exec "$c" node -e 'const f=require("node:fs"), p=process.env.OPENCLAW_WORKSPACE_DIR; f.mkdirSync(p,{recursive:true}); f.writeFileSync(p+"/catalog-e2e.md","kept-through-upgrade\n");' || fail "OpenClaw can't write its workspace"
    # OpenClaw's config accepts JSON5. A comment retained by its editor must not
    # stop the next image's startup from reading and merging that config.
    docker exec "$c" node -e 'const f=require("node:fs"), p=process.env.OPENCLAW_CONFIG_PATH; f.writeFileSync(p,"// retained through upgrade\n"+f.readFileSync(p,"utf8"));' || fail "OpenClaw can't keep JSON5 config"
  fi
  docker exec "$c" node -e 'const f=require("node:fs"); if(f.readFileSync(process.env.OPENCLAW_WORKSPACE_DIR+"/catalog-e2e.md","utf8")!=="kept-through-upgrade\n")process.exit(1);' || fail "OpenClaw lost its workspace through upgrade"
  said=$(docker exec "$c" node /app/dist/index.js gateway call config.get --url ws://127.0.0.1:18789 --token "$token" --timeout 10000 --json) || fail "OpenClaw can't read its config through the authenticated gateway"
  jq -e --arg port "$port" '.config.gateway.auth.mode == "token" and .config.gateway.mode == "local" and .config.gateway.controlUi.allowedOrigins == ["http://localhost:" + $port, "http://127.0.0.1:" + $port]' <<<"$said" >/dev/null || fail "OpenClaw lost its token gate or explicit control-UI origins"
}

# LobeChat's browser edition stores no chats on the server. Try both versions
# against a local model-list fixture, with no real model key or external call.
check_lobechat() {
  local port c code payload said status
  port=$(value PORT)
  c=$(container lobechat)
  code=$(generated lobechat ACCESS_CODE)
  answers "http://127.0.0.1:$port/"
  docker exec -d "$c" /bin/node -e 'require("node:http").createServer((q,r)=>{r.setHeader("content-type","application/json");r.end(JSON.stringify({object:"list",data:[{id:"gpt-4o-mini",object:"model",created:0,owned_by:"catalog-e2e"}]}));}).listen(19001,"127.0.0.1");' || fail "can't start LobeChat's local provider fixture"
  docker exec "$c" /bin/node -e 'fetch("http://127.0.0.1:19001/v1/models").then(r=>process.exit(r.ok?0:1),()=>process.exit(1));' || fail "LobeChat's local provider fixture doesn't answer"
  payload=$(docker exec -e CATALOG_CODE=not-the-access-code "$c" /bin/node -e 'const b=Buffer.from(JSON.stringify({accessCode:process.env.CATALOG_CODE,baseURL:"http://127.0.0.1:19001/v1"})),k=Buffer.from("LobeHub · LobeHub"); for(let i=0;i<b.length;i++)b[i]^=k[i%k.length]; console.log(b.toString("base64"));') || fail "can't build LobeChat's browser auth header"
  status=$(curl -sS --max-time 20 -o /dev/null -w '%{http_code}' -H "X-lobe-chat-auth: $payload" "http://127.0.0.1:$port/webapi/models/openai") || fail "LobeChat's model route doesn't answer"
  [[ "$status" == 401 ]] || fail "LobeChat answers $status to a wrong access code, want 401"
  payload=$(docker exec -e CATALOG_CODE="$code" "$c" /bin/node -e 'const b=Buffer.from(JSON.stringify({accessCode:process.env.CATALOG_CODE,baseURL:"http://127.0.0.1:19001/v1"})),k=Buffer.from("LobeHub · LobeHub"); for(let i=0;i<b.length;i++)b[i]^=k[i%k.length]; console.log(b.toString("base64"));') || fail "can't build LobeChat's browser auth header"
  said=$(curl -fsS --max-time 20 -H "X-lobe-chat-auth: $payload" "http://127.0.0.1:$port/webapi/models/openai") || fail "LobeChat doesn't take its generated access code"
  jq -e 'any(.id == "gpt-4o-mini")' <<<"$said" >/dev/null || fail "LobeChat can't list models from its local provider"
}

# The version before the one each entry pins, which it upgrades from, and the images
# that version ran in place of the entry's: its version, then each image by digest.
declare -A previous=(
  [pgadmin]="9.16 dpage/pgadmin4:9.16@sha256:40fa840c5bb7c8463957f1255b01283732c2d8c9396a956d180f8e6c296753b3"
  [redis]="8.10.1 redis:8.10.1@sha256:8a1efc5f479551822b47424ccae982026b633f28818eab0387348120a61e10e2"
  [mariadb]="12.3.2 mariadb:12.3.2@sha256:a02fe89cb597d4375812b2eac90cf9d0775d4686daa7f7cc750ebbcad7525bbc"
  [coder]="2.38.2 ghcr.io/coder/coder:v2.38.2@sha256:ab17fc8e172e0f489cc1a67f513a506a49583ff692835601a614e20bb83ad34a"
  [uptime-kuma]="2.5.7 louislam/uptime-kuma:2.5.7@sha256:4b95dbe7ac5343b768f8e04e3035ccd59997872b1a35ab9b0f082d2cc279070e"
  [n8n]="2.44.6 n8nio/n8n:2.44.6@sha256:d02752c6ab446ae9db8deec2f9cc887c378f77cdcad31650814df936b7386dfb"
  [gitea]="28.1.0 gitea/gitea:28.1.0@sha256:f505a0d3a41323b786e05942fc7a4075e07ae7c169fd9485649aa55ee543a21a"
  [vaultwarden]="1.37.3 vaultwarden/server:1.37.3@sha256:1587c45feaa479f1f5e8af3b00eded36bff77bcf1880cf8dbf0541706dd470e0"
  [nextcloud]="35.0.1 nextcloud:35.0.1-apache@sha256:8f2f0e93aca5e9457019933936dea1f313052e72c696b4aecb31580a208b859f"
  [rustfs]="1.0.2 rustfs/rustfs:1.0.2@sha256:7e948dad4748bda48f8985fda7195f8cf2e4259427011f6120acc2c9acf51587"
  [grafana]="13.2.3 grafana/grafana:13.2.3@sha256:b28bae15e219c998fb0e0424ed724930cc61b1f61fb404d47c862f9a23f9e572"
  [umami]="3.4.0 ghcr.io/umami-software/umami:3.4.0@sha256:85909afc45bdcda1917394594a087421fdbb05610fded0fa9f6fb861abb2f367"
  [plausible]="3.2.0 ghcr.io/plausible/community-edition:v3.2.0@sha256:3d8af8d4a1be32c3dcc5e8dc01496639d945edd9963827e18abe984dd6bda086"
  [ghost]="6.71.0 ghost:6.71.0@sha256:00032f15726204565bf799ac272ee6fef588547cce50bedd38de3db55272c053"
  [wordpress]="7.1.2 wordpress:7.1.2-apache@sha256:e04cf657f24889e96b5544828cdd5a92a0b51819f3e1899183f9be6ea0485edf"
  [memos]="0.30.0 neosmemo/memos:0.30.0@sha256:71a5b4738d1bed96e92112004054f0888e92791b64eb78afd79077c96e6f9327"
  [adminer]="6.1.2 adminer:6.1.2@sha256:251295c1cd51d3889f1c42ffa4864b24eeab51f0caf395cfe5bf4726c5ec9a2a"
  [jellyfin]="12.2 jellyfin/jellyfin:12.2@sha256:357724bf0ae27a672c7cbaa899db2d9abeb13dbd8657ccce750258a4c059d037"
  [home-assistant]="2026.10.3 ghcr.io/home-assistant/home-assistant:2026.10.3@sha256:407b89cd11a8cb67d388d21739a226c74acbbebe1ac59e3b83b11b43ce76b98f"
  [openclaw]="2026.10.6 ghcr.io/openclaw/openclaw:2026.10.6@sha256:7dbff0fe5f5359b1badefe90a23efa9f203cd0d3c36bee095f4896c7a1d3864b"
  [hermes-agent]="0.22.4 nousresearch/hermes-agent:v0.22.4@sha256:7a69201726bc64db2072a1371557137c23312d29705cda2b8ea2a0214a50a4cf"
  [new-api]="0.13.1-patch.1 calciumion/new-api:v0.13.1-patch.1@sha256:95fab02db3925f8028f8f43b333734893a3c568ac555fa3b5df74650286d9ce3"
  [lobechat]="1.143.2 lobehub/lobe-chat:1.143.2@sha256:65793dfe2378eecea89c335c01cb1094c240eb67047902859140e0f0b1a15069"
  [sftpgo]="2.7.5 drakkan/sftpgo:v2.7.5@sha256:9011fe608d336d3daf6ed6224b16fd40443aab8f3335e0b20853d6a539c58738"
  [openlist]="4.2.7 openlistteam/openlist:v4.2.7@sha256:11b490123d57f88c84ecda46285e1921869cf511da3e5bb4062f183e7e1200b7"
  [gotify]="3.1.0 gotify/server:3.1.0@sha256:be44495e4609c6f5b06c01a9445ec6198dba902ce49fc3d0d71ab585cda8014c"
  [linkding]="1.46.2 sissbruecker/linkding:1.46.2@sha256:0c0a9a04c7ebd644293c37f98710872fe7cdff0ea1ff7fd9be4d235e4c4678d2"
)
# What a person types for an entry, as dockwell catalog install takes it.
declare -A given=(
  [pgadmin]="ADMIN_EMAIL=owner@example.com"
  [nextcloud]="DOMAIN=cloud.example.com"
  [ghost]="URL=https://blog.example.com"
  [jellyfin]="MEDIA_DIR=$media_dir"
  [lobechat]="OPENAI_API_KEY"
)
# The companions an entry's check signs in through, which an install leaves out unless asked (ADR 0076).
declare -A companions=(
  [mariadb]="phpmyadmin"
)
# Secret inputs use their names above and values in the CLI's environment, including under sudo.
declare -A given_env=(
  [lobechat]="OPENAI_API_KEY=catalog-test-provider-key-never-sent-outside-this-container"
)

# release writes an entry into the catalog's directory, which takes over from the
# built-in one: as it is, at a higher release, or as the version before it.
release() {
  local entry=$1 number=$2 to=${3:-} ref repo
  shift 3 || shift $#
  if [[ -n "$to" ]]; then
    entry=$(jq --arg v "$to" '.version = $v' <<<"$entry")
    for ref in "$@"; do
      repo=${ref%%@*}
      repo=${repo%:*}
      entry=$(jq --arg repo "$repo" --arg ref "$ref" '
        .compose |= (split("\n") | map(if test("^\\s*image: " + ($repo | gsub("[.]"; "\\.")) + "[:@]") then sub("image: \\S+"; "image: " + $ref) else . end) | join("\n"))' <<<"$entry")
      [[ "$(jq -r .compose <<<"$entry")" == *"image: $ref"* ]] || fail "the entry $id runs no image of $repo for the version before"
    done
  fi
  jq --argjson n "$number" '.release = $n' <<<"$entry" >"$catalog_dir/$id.yaml"
}

# settled waits for every container of the app to pass its health check, and says
# each runs an image pinned by digest.
settled() {
  local names health image
  names=$(docker ps --all --filter "label=com.docker.compose.project=$project" --format '{{.Names}}') || fail "docker ps failed"
  [[ -n "$names" ]] || fail "$id runs nothing once its install has applied"
  for c in $names; do
    image=$(docker inspect --format '{{.Config.Image}}' "$c") || fail "docker inspect $c failed"
    [[ "$image" == *@sha256:* ]] || fail "$c runs $image, not pinned by digest"
    [[ -z "${1:-}" || "$1" == *"image: $image"* ]] || fail "$c runs $image, which its entry doesn't name"
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$c") || fail "docker inspect $c failed"
    [[ "$health" == healthy ]] || fail "$c reads $health once the change has applied, want healthy: its entry needs a health check that proves it answers"
  done
  # Every port of every service answers only on this server until it has a domain, and
  # what a person opens has one.
  local web bound
  for c in $names; do
    bound=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$c") || fail "docker inspect $c failed"
    jq -e '[(. // {})[][] | .HostIp] | all(. == "127.0.0.1")' <<<"$bound" >/dev/null || fail "$c is published past this server: $bound"
  done
  web=$(jq -r '.web.service // empty' <<<"$entry")
  if [[ -n "$web" ]]; then
    bound=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$(container "$web")") || fail "docker inspect $web failed"
    jq -e '[(. // {})[][] | .HostIp] | length > 0' <<<"$bound" >/dev/null || fail "$web publishes no port: $bound"
  fi
}

# secrets_of prints each secret a container uses, as the variable it sees it as and
# the secret's name, from the label Dockwell writes on it.
secrets_of() {
  local label
  label=$(docker inspect --format '{{index .Config.Labels "dockwell.uses"}}' "$1") || fail "docker inspect $1 failed"
  tr ',' '\n' <<<"$label" | sed -n 's/^\([^=]*\)=secret:\([^@]*\)@.*/\1 \2/p'
}
# held prints what each container of the app has for each secret it uses: its
# service, the secret's name and its value.
held() {
  local c service as name
  for c in $(docker ps --filter "label=com.docker.compose.project=$project" --format '{{.Names}}'); do
    service=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$c") || fail "docker inspect $c failed"
    while read -r as name; do
      [[ -z "$as" ]] || echo "$service $name $(env_of "$c" "$as")"
    done < <(secrets_of "$c")
  done | sort -u
}

# sealed checks that nothing anyone can read without a passkey holds a secret the app
# has: what the API says of the install, and each container's log, but for a secret
# its entry says the app writes there.
sealed() {
  local seen service name value logged c logs plan holds
  # Each read joined with &&, as $(a; b) would pass on a failing a.
  seen=$(dw changes list --json && dw catalog installs --json && dw compose export --project "$installed" && dw resources list --json &&
    dw services list --project "$installed" --environment production --json && dw events list --json) || fail "reading the install back failed"
  for plan in "${plans[@]}"; do
    seen+=$(dw changes get "$plan" --json) || fail "reading the plan $plan back failed"
  done
  holds=$(held) || fail "reading what $id's containers hold failed"
  while read -r service name value; do
    [[ -n "$service" ]] || continue
    [[ -n "$value" ]] || fail "$service uses the secret $name, and has no value for it"
    [[ "$seen" != *"$value"* ]] || fail "what anyone can read of $id holds its secret $name"
    logged=$(jq -r --arg s "$name" '[.inputs[]? | select(.name == $s) | .logged // false] | any' <<<"$entry")
    c=$(container "$service")
    logs=$(docker logs "$c" 2>&1) || fail "docker logs $c failed"
    [[ "$logged" == true || "$logs" != *"$value"* ]] || fail "$service writes the secret $name in its log, which takes no passkey to read"
  done <<<"$holds"
  [[ -n "$holds" ]] || [[ "$(jq '[.inputs[]? | select(.type == "INPUT_TYPE_SECRET")] | length' <<<"$entry")" == 0 ]] || fail "$id uses none of its secrets"
}

# plan_install plans installing $id with its flags, and sets planned, change, installed
# (the install's project, which is <id>-2 once a rolled-back install has used <id>),
# environment and project (its Compose project) from the plan. Each plan goes on the
# list a secret mustn't be in, and its project on the list cleanup removes, a plan a
# registry failed included.
plan_install() {
  planned=$(dw catalog install "$id" "${flags[@]}" --plan-only --json) || fail "planning to install $id failed"
  change=$(jq -r '.change.id // empty' <<<"$planned")
  [[ "$change" == chg_* ]] || fail "installing $id planned no change: $planned"
  plans+=("$change")
  installed=$(jq -r '[.change.plan.resources[] | select(.resource.kind == "project")][0].resource.name // empty' <<<"$planned")
  [[ -n "$installed" ]] || fail "installing $id planned no project: $planned"
  environment=$(jq -r '[.change.plan.resources[] | select(.resource.kind == "service")][0].resource.scope' <<<"$planned")
  project=${environment,,}
  projects+=("$project")
}
# plan_upgrade plans upgrading the install, and sets planned and change.
plan_upgrade() {
  planned=$(dw catalog upgrade "$installed" --plan-only --json) || fail "planning to upgrade $installed failed"
  change=$(jq -r '.change.id // empty' <<<"$planned")
  [[ "$change" == chg_* ]] || fail "upgrading $id planned no change: $planned"
  plans+=("$change")
}

for id in "$@"; do
  started=$SECONDS
  declare -F "check_$id" >/dev/null || fail "there's no check_$id in this script, which says what answering means for $id. Write one"
  entry=$(dw catalog get "$id" --json | jq .entry) || fail "the catalog has no $id"
  [[ "$entry" != null ]] || fail "the catalog has no $id"
  version=$(jq -r .version <<<"$entry")
  read -r -a values <<<"${given[$id]:-}"
  read -r -a input_env <<<"${given_env[$id]:-}"
  flags=()
  for v in "${values[@]}"; do flags+=(--values "$v"); done
  for c in ${companions[$id]:-}; do flags+=(--companions "$c=true"); done
  before=${previous[$id]:-}

  if [[ -n "$before" ]]; then
    read -r -a was <<<"$before"
    step "$id: ${was[0]} installs, answers and signs in"
    release "$entry" 1 "${was[@]}"
  else
    step "$id: $version installs, answers and signs in"
  fi
  plans=()
  plan_install
  apply_patiently plan_install
  if [[ -n "$before" ]]; then
    settled
    version=${was[0]} "check_$id" first

    step "$id: upgrades to $version in one plan, keeping its data and its secrets"
    release "$entry" 2
    upgrade=$(dw catalog installs --json | jq -r --arg p "$installed" '.installs[] | select(.project == $p) | .upgrade.version // empty') || fail "catalog installs failed"
    [[ "$upgrade" == "$version" ]] || fail "$version doesn't read as an upgrade of $id: $(dw catalog installs --json)"
    secrets_before=$(held)
    plan_upgrade
    if jq -e '.change.plan.resources | any(.resource.kind == "volume")' <<<"$planned" >/dev/null || [[ "$(jq -r .compose <<<"$entry")" == *volumes:* ]]; then
      jq -e '(.change.plan.notes | any(.reason == "backup_first")) and (.change.plan.servers[0].steps | any(.backupFirst != null))' <<<"$planned" >/dev/null ||
        fail "the upgrade of $id doesn't back its volumes up first: $planned"
    fi
    apply_patiently plan_upgrade
    rm -f "$catalog_dir/$id.yaml"
    secrets_after=$(held)
    [[ "$secrets_after" == "$secrets_before" ]] || fail "an upgrade of $id changed its secrets"
    settled "$(jq -r .compose <<<"$entry")"
    "check_$id" upgraded
  else
    settled "$(jq -r .compose <<<"$entry")"
    "check_$id" first
  fi
  sealed

  step "$id: goes through a plan, and its volumes stay for an undo"
  all=$(dw resources list --json) || fail "listing resources failed"
  edits=$(jq -c --arg env "$environment" --arg p "$installed" '[.resources[] | select(.resource.scope == $env) | {resource: .resource, delete: true}] + [{resource: {kind: "project", name: $p}, delete: true}] | .[]' <<<"$all")
  flags=()
  while read -r e; do flags+=(--edits "$e"); done <<<"$edits"
  planned=$(dw changes propose --summary "Remove $id" "${flags[@]}" --plan-only --json) || fail "planning to remove $id failed"
  apply "$(jq -r .change.id <<<"$planned")"
  left=$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project") || fail "docker ps failed"
  [[ -z "$left" ]] || fail "removing $id left containers: $(docker ps --all --filter "label=com.docker.compose.project=$project" --format '{{.Names}} {{.Status}}')"
  dw catalog installs --json | jq -e --arg p "$installed" '.installs // [] | any(.project == $p) | not' >/dev/null || fail "$installed still reads as installed"
  mapfile -t volumes < <(jq -r '.compose' <<<"$entry" | sed -n '/^volumes:/,$s/^  \([a-z0-9_-]*\):.*/\1/p')
  if jq -r '.compose' <<<"$entry" | grep -q '^volumes:'; then
    ((${#volumes[@]} > 0)) || fail "the entry $id has volumes:, and this found none in it to check"
  fi
  for volume in "${volumes[@]}"; do
    docker volume inspect "${project}_$volume" >/dev/null 2>&1 || fail "removing $id took its volume $volume at once, which an undo needs for 7 days"
  done
  echo "$id took $((SECONDS - started)) s"
  project=""
done

echo "catalog-e2e: ok"
