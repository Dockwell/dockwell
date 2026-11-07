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
# real install. It needs Docker, Go, curl and jq, and the ports the apps' entries
# suggest. With E2E_ROOT set, as in CI, Dockwell runs as root through sudo, as it does
# on a server.
#
#   scripts/catalog-e2e.sh bin/dockwell pgadmin redis
set -euo pipefail
# Anything this starts asks nothing beyond this machine (DOCKWELL_TEST, ADR 0034).
export DOCKWELL_TEST=1

usage="usage: scripts/catalog-e2e.sh path/to/dockwell ENTRY..."
for tool in docker curl go jq; do
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
projects=()
plans=()
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
  [[ -z "$pid" ]] || priv kill "$pid" 2>/dev/null || true
  wait 2>/dev/null || true
  priv rm -rf "$data" "$log" "$cookie" "$passkey" "$stepup" "$catalog_dir"
}
trap cleanup EXIT

# project is the Compose project of the app being tried, which its containers carry.
project=""
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
dw() { priv env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text "$bin" "$@"; }

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
wait_applied() {
  local state=""
  for _ in $(seq 1200); do
    state=$(change_field "$1" state) || state=""
    [[ "$state" == CHANGE_STATE_APPLIED ]] && return 0
    [[ "$state" == CHANGE_STATE_FAILED ]] && fail "$1 failed: $(dw changes get "$1" --json)"
    sleep 0.5
  done
  fail "$1 is $state after 10 min, want applied"
}
# apply applies a planned change: with the CLI's token when the plan lets one, and as
# a person does in the panel when it destroys data or reaches the server, confirming
# with the passkey the claim made when it's as good as root.
apply() {
  local plan fingerprint answer proof
  plan=$(dw changes get "$1" --json) || fail "can't read $1"
  if jq -e '.change.plan | (.destructive or .hostAccess) | not' <<<"$plan" >/dev/null; then
    dw changes apply "$1" --yes >/dev/null || fail "applying $1 failed: $(dw changes get "$1" --json)"
    return 0
  fi
  fingerprint=$(jq -r .change.plan.fingerprint <<<"$plan")
  answer=$(person_answer ChangeService/ApplyChange "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\"}") || fail "a person can't reach the API to apply $1"
  if [[ "${answer##*$'\n'}" == 403 ]] && grep -qi '^dockwell-error: needs_step_up' <<<"$answer"; then
    proof=$("$stepup" "http://localhost:$port" "$cookie" "$passkey" STEP_UP_ACTION_APPLY_ROOT_CHANGE "$1") || fail "can't confirm $1 with the passkey"
    answer=$(person_answer ChangeService/ApplyChange "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\",\"proof\":\"$proof\"}") || fail "a person can't reach the API to apply $1"
  fi
  [[ "${answer##*$'\n'}" == 200 ]] || fail "a person can't apply $1: $answer"
  wait_applied "$1"
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
  got=$(jq -r --arg p "$id" --arg k "$1" '.installs[] | select(.project == $p) | .pin.values[$k] // empty' <<<"$installs")
  [[ -n "$got" ]] || fail "the install $id keeps no $1: $installs"
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

# The version before the one each entry pins, which it upgrades from, and the images
# that version ran in place of the entry's: its version, then each image by digest.
declare -A previous=(
  [pgadmin]="9.16 dpage/pgadmin4:9.16@sha256:40fa840c5bb7c8463957f1255b01283732c2d8c9396a956d180f8e6c296753b3"
  [redis]="8.10.1 redis:8.10.1@sha256:8a1efc5f479551822b47424ccae982026b633f28818eab0387348120a61e10e2"
  [mariadb]="12.3.2 mariadb:12.3.2@sha256:a02fe89cb597d4375812b2eac90cf9d0775d4686daa7f7cc750ebbcad7525bbc"
  [coder]="2.38.2 ghcr.io/coder/coder:v2.38.2@sha256:ab17fc8e172e0f489cc1a67f513a506a49583ff692835601a614e20bb83ad34a"
)
# What a person types for an entry, as dockwell catalog install takes it.
declare -A given=(
  [pgadmin]="ADMIN_EMAIL=owner@example.com"
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
  # What a person opens answers only on this server until it has a domain.
  local web bound
  web=$(jq -r '.web.service // empty' <<<"$entry")
  if [[ -n "$web" ]]; then
    bound=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$(container "$web")") || fail "docker inspect $web failed"
    jq -e '[.[][] | .HostIp] | length > 0 and all(. == "127.0.0.1")' <<<"$bound" >/dev/null || fail "$web is published past this server: $bound"
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
  seen=$(dw changes list --json && dw catalog installs --json && dw compose export --project "$id" && dw resources list --json &&
    dw services list --project "$id" --environment production --json && dw events list --json) || fail "reading the install back failed"
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

for id in "$@"; do
  started=$SECONDS
  declare -F "check_$id" >/dev/null || fail "there's no check_$id in this script, which says what answering means for $id. Write one"
  entry=$(dw catalog get "$id" --json | jq .entry) || fail "the catalog has no $id"
  [[ "$entry" != null ]] || fail "the catalog has no $id"
  version=$(jq -r .version <<<"$entry")
  read -r -a values <<<"${given[$id]:-}"
  flags=()
  for v in "${values[@]}"; do flags+=(--values "$v"); done
  before=${previous[$id]:-}

  if [[ -n "$before" ]]; then
    read -r -a was <<<"$before"
    step "$id: ${was[0]} installs, answers and signs in"
    release "$entry" 1 "${was[@]}"
  else
    step "$id: $version installs, answers and signs in"
  fi
  planned=$(dw catalog install "$id" "${flags[@]}" --plan-only --json) || fail "planning to install $id failed"
  change=$(jq -r '.change.id // empty' <<<"$planned")
  [[ "$change" == chg_* ]] || fail "installing $id planned no change: $planned"
  plans=("$change")
  environment=$(jq -r '[.change.plan.resources[] | select(.resource.kind == "service")][0].resource.scope' <<<"$planned")
  project=${environment,,}
  projects+=("$project")
  apply "$change"
  if [[ -n "$before" ]]; then
    settled
    version=${was[0]} "check_$id" first

    step "$id: upgrades to $version in one plan, keeping its data and its secrets"
    release "$entry" 2
    upgrade=$(dw catalog installs --json | jq -r --arg p "$id" '.installs[] | select(.project == $p) | .upgrade.version // empty') || fail "catalog installs failed"
    [[ "$upgrade" == "$version" ]] || fail "$version doesn't read as an upgrade of $id: $(dw catalog installs --json)"
    secrets_before=$(held)
    planned=$(dw catalog upgrade "$id" --plan-only --json) || fail "planning to upgrade $id failed"
    if jq -e '.change.plan.resources | any(.resource.kind == "volume")' <<<"$planned" >/dev/null || [[ "$(jq -r .compose <<<"$entry")" == *volumes:* ]]; then
      jq -e '(.change.plan.notes | any(.reason == "backup_first")) and (.change.plan.servers[0].steps | any(.backupFirst != null))' <<<"$planned" >/dev/null ||
        fail "the upgrade of $id doesn't back its volumes up first: $planned"
    fi
    plans+=("$(jq -r .change.id <<<"$planned")")
    apply "${plans[-1]}"
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
  edits=$(jq -c --arg env "$environment" --arg id "$id" '[.resources[] | select(.resource.scope == $env) | {resource: .resource, delete: true}] + [{resource: {kind: "project", name: $id}, delete: true}] | .[]' <<<"$all")
  flags=()
  while read -r e; do flags+=(--edits "$e"); done <<<"$edits"
  planned=$(dw changes propose --summary "Remove $id" "${flags[@]}" --plan-only --json) || fail "planning to remove $id failed"
  apply "$(jq -r .change.id <<<"$planned")"
  left=$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project") || fail "docker ps failed"
  [[ -z "$left" ]] || fail "removing $id left containers: $(docker ps --all --filter "label=com.docker.compose.project=$project" --format '{{.Names}} {{.Status}}')"
  dw catalog installs --json | jq -e --arg p "$id" '.installs // [] | any(.project == $p) | not' >/dev/null || fail "$id still reads as installed"
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
