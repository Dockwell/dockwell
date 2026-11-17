#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Proves apps of a catalog on this machine's Docker Engine, through the CLI, as a
# person would (ADR 0100). Each entry named installs from a git checkout of the
# catalog, as a project of its own, with its secrets generated and sealed: first at
# the release before, as --base has it or else the catalog's history, and again at
# the first release of its current major, each of which then upgrades to the entry in
# one plan that backs its volumes up first, keeping its data and its secrets. An
# entry with neither installs as it is. Each container turns healthy, runs an image
# pinned by digest, and the app answers and signs in with what Dockwell generated,
# which nothing anyone can read holds. Then it goes again through a plan, and its
# volumes stay on the server for an undo, as the PRD keeps what a deletion removes
# for 7 days.
#
# What answering means for an app is the catalog's checks/<id>.sh, which defines
# check_<id>. Each check runs as its own process, from a fresh copy of itself and of
# this script, which read both before any check ran; and an entry passes on this
# script's own probes after its check, not on the check's exit alone: its containers
# healthy on the images its entry names, what it opens answering, each database it
# keeps holding a sentinel through the upgrade, and its secrets sealed. An entry
# without a check gets those probes alone, the floor. It's listed in the file
# E2E_FLOORED names, as an entry with the floor alone doesn't merge by itself.
#
# Both repositories run it through .github/workflows/catalog-proof.yml. It uses
# throwaway data directories and removes what it made, so it never touches a real
# install. It needs Docker, Go, git, curl, jq and openssl, and the ports the apps'
# entries suggest. SFTPGo also needs sshpass and OpenSSH's sftp and ssh-keyscan.
# With E2E_ROOT set, as in CI, Dockwell runs as root through sudo, as it does on a
# server.
#
#   scripts/catalog-e2e.sh bin/dockwell --catalog ../catalog --base origin/main pgadmin redis
set -euo pipefail
# Anything this starts asks nothing beyond this machine (DOCKWELL_TEST, ADR 0034).
export DOCKWELL_TEST=1

# It runs from a copy of itself that nothing else holds, made before any check runs,
# and each check from a fresh copy of that, so a check that rewrites this file reaches
# neither the run that judges it nor the next entry (#726's slice 2 re-check, N3).
if [[ "${1:-}" != --check && "${1:-}" != --from-copy ]]; then
  copy=$(mktemp -d)
  cp "${BASH_SOURCE[0]}" "$copy/catalog-e2e.sh"
  chmod 0444 "$copy/catalog-e2e.sh"
  chmod 0555 "$copy"
  exec bash "$copy/catalog-e2e.sh" --from-copy "$copy" "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" "$@"
fi

usage="usage: scripts/catalog-e2e.sh path/to/dockwell --catalog <checkout> [--at <commit>] [--base <commit>] [--require-upgrade] [--no-floor] ENTRY..."
check_phase=""
if [[ "${1:-}" == --check ]]; then
  # One phase of one entry's check, as its own process. What it needs comes in its
  # environment, from the run that judges it, and is set read-only just before the
  # check is sourced, past everything this script sets for both.
  check_phase=${2:?} check_file=${3:?}
  plans=()
  projects=()
  pid=""
else
  # The copy it runs from, which it removes at the end, and Dockwell's checkout.
  copy=${2:?} dockwell=${3:?}
  shift 3
  trap 'chmod -R u+w "$copy"; rm -rf "${copy:?}"' EXIT
  for tool in docker curl git go jq openssl; do
    command -v "$tool" >/dev/null || {
      echo "catalog-e2e: $tool isn't installed, and the script needs it. Install it, such as with apt-get install $tool, then run it again" >&2
      exit 1
    }
  done
  bin=$(realpath "${1:?$usage}")
  shift
  catalog="" at=HEAD base="" require_upgrade=0 no_floor=0
  while (($# > 0)); do
    case "$1" in
    --catalog) catalog=$(realpath "${2:?$usage}") && shift 2 ;;
    --at) at=${2:?$usage} && shift 2 ;;
    --base) base=${2:?$usage} && shift 2 ;;
    --require-upgrade) require_upgrade=1 && shift ;;
    --no-floor) no_floor=1 && shift ;;
    *) break ;;
    esac
  done
  [[ -n "$catalog" && -d "$catalog/entries" ]] || {
    echo "$usage: --catalog names a git checkout of the catalog, with its history" >&2
    exit 1
  }
  # A catalog with no checks at all is one before ADR 0100's slice 2, which would prove
  # every entry with the floor alone.
  [[ -d "$catalog/checks" ]] || {
    echo "catalog-e2e: $catalog has no checks/, which says what answering means for each app. Prove a commit of the catalog that has it" >&2
    exit 1
  }
  (($# > 0)) || {
    echo "$usage" >&2
    exit 1
  }
  addr=127.0.0.1:${E2E_PORT:-7698}
  # Dockwell's own port, which a check's local port can't shadow.
  dockwell_port=${addr##*:}
  data=$(mktemp -d)
  log=$(mktemp)
  cookie=$(mktemp)
  passkey=$(mktemp)
  stepup=$(mktemp)
  catalog_tool=$(mktemp)
  # A release other than the one built in comes from here, which Dockwell reads each time.
  catalog_dir=$(mktemp -d)
  chmod 755 "$catalog_dir"
  # A folder of media, for an app that reads one from the server.
  media_dir=$(mktemp -d)
  echo kept >"$media_dir/e2e.txt"
  chmod -R a+rX "$media_dir"
  # What this reads of each entry before any check runs, and the work of each check's
  # process: a fresh copy of the harness and the check, and the state it keeps from
  # its first phase to its second.
  work=$(mktemp -d)
  # The Compose projects of what this made, for cleanup.
  projects=()
  plans=()
  input_file=""
  pid=""
fi

as_dockwell=()
[[ -z "${E2E_ROOT:-}" ]] || as_dockwell=(sudo -n)
priv() { "${as_dockwell[@]}" "$@"; }

cleanup() {
  local p
  for p in "${projects[@]}"; do
    docker ps --all --quiet --filter "label=com.docker.compose.project=$p" | xargs --no-run-if-empty docker rm --force --volumes >/dev/null 2>&1 || true
    docker volume ls --quiet --filter "label=com.docker.compose.project=$p" | xargs --no-run-if-empty docker volume rm --force >/dev/null 2>&1 || true
    docker network ls --quiet --filter "label=com.docker.compose.project=$p" | xargs --no-run-if-empty docker network rm >/dev/null 2>&1 || true
  done
  docker ps --all --quiet --filter label=dev.dockwell.volume-tool | xargs --no-run-if-empty docker rm --force >/dev/null 2>&1 || true
  docker rm --force e2e-adminer-db >/dev/null 2>&1 || true
  [[ -z "$pid" ]] || priv kill "$pid" 2>/dev/null || true
  wait 2>/dev/null || true
  chmod -R u+w "$work" "$copy" 2>/dev/null || true
  priv rm -rf "$data" "$log" "$cookie" "$passkey" "$stepup" "$catalog_tool" "$catalog_dir" "$media_dir" "$work" "$copy"
}
[[ -n "$check_phase" ]] || trap cleanup EXIT

# project is the Compose project of the app being tried, which its containers carry,
# and installed its Dockwell project, from its install's plan.
project=""
installed=""
fail() {
  echo "catalog-e2e: $*" >&2
  # A check's process says why it failed; the run that judges it shows the rest.
  [[ -z "$check_phase" ]] || exit 1
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
# The values a check or the floor gives as secrets reach the CLI's environment from
# input_file, mode 0600, never as arguments, which ps shows and sudo logs (#745's
# review, L2).
dw() {
  if [[ -n "${input_file:-}" ]]; then
    # shellcheck disable=SC2016 # the inner shell reads its own arguments.
    priv bash -c 'set -a && . "$1" && set +a && shift && exec "$@"' dw "$input_file" \
      env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text "$bin" "$@"
    return
  fi
  priv env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text "$bin" "$@"
}
# inputs_file writes the NAME=value pairs given_env_of holds into a fresh input_file,
# mode 0600, or leaves none.
inputs_file() {
  local pair
  input_file=""
  [[ -n "$given_env_of" ]] || return 0
  # Space between pairs and nothing else, so a value can't add a line root sources.
  [[ "$given_env_of" != *[$'\n\t\r']* ]] || fail "$id's given_env holds a line break or a tab: give each NAME=value with no whitespace in it"
  local -a pairs
  IFS=' ' read -r -a pairs <<<"$given_env_of"
  for pair in "${pairs[@]}"; do
    [[ "$pair" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] || fail "$id's given_env has a part that isn't NAME=value: give each as NAME=value, apart by spaces"
  done
  input_file=$(umask 077 && mktemp "$work/inputs.XXXXXX")
  for pair in "${pairs[@]}"; do
    printf '%s=%q\n' "${pair%%=*}" "${pair#*=}"
  done >"$input_file"
}


# person_answer calls the API as a signed-in person does, from the panel's origin, and
# says what it answered, its headers first, and the status last.
person_answer() {
  curl -sS --max-time 30 -D - -w '\n%{http_code}' --resolve "localhost:$dockwell_port:127.0.0.1" -H "Origin: http://localhost:$dockwell_port" -H 'Content-Type: application/json' \
    -H "Cookie: $(cat "$cookie")" -d "$2" "http://localhost:$dockwell_port/dockwell.v1.$1"
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
    proof=$("$stepup" "http://localhost:$dockwell_port" "$cookie" "$passkey" STEP_UP_ACTION_APPLY_ROOT_CHANGE "$1") || fail "can't confirm $1 with the passkey"
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

# Each app's own check is the catalog's, as checks/<id>.sh, which a process of its
# own sources (ADR 0100): check_<id> first, once it's installed, and check_<id>
# upgraded, once it upgraded from an earlier release, with version the release it
# checks. It answers, signs in with what Dockwell generated, and after an upgrade
# finds what it wrote.

# kept holds what an app's check wrote or read before an upgrade, by the app, for the
# check after it, which its process keeps in the entry's state for the next.
declare -A kept=()
# sentinels holds what the harness's own probe wrote to each database before an
# upgrade, by the entry and service, in the run that judges it, where no check is.
declare -A sentinels=()

# engine_of says which engine an image is, of those the floor writes to: postgres,
# mysql (MariaDB too), redis (Valkey too) or mongo. It fails for any other.
engine_of() {
  local name=${1%%@*}
  name=${name%:*}
  name=${name##*/}
  case "$name" in
  postgres | postgis) echo postgres ;;
  mariadb | mysql) echo mysql ;;
  redis | valkey) echo redis ;;
  mongo) echo mongo ;;
  *) return 1 ;;
  esac
}

# sentinel writes a value to a database with the engine's own client in its
# container, or reads it back: sentinel <engine> <container> write <value> | read.
# MySQL and MariaDB's root signs in over TCP, as the mariadb check does after
# Dockwell gives root a new password.
sentinel() {
  local engine=$1 c=$2 what=$3 value=${4:-}
  case "$engine/$what" in
  postgres/write) docker exec -i "$c" sh -c 'exec psql -v ON_ERROR_STOP=1 -qtAX -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-${POSTGRES_USER:-postgres}}"' \
    <<<"CREATE TABLE IF NOT EXISTS dockwell_proof (v text); INSERT INTO dockwell_proof VALUES ('$value');" ;;
  postgres/read) docker exec -i "$c" sh -c 'exec psql -v ON_ERROR_STOP=1 -qtAX -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-${POSTGRES_USER:-postgres}}"' \
    <<<"SELECT v FROM dockwell_proof;" ;;
  mysql/write) docker exec -i "$c" sh -c 'MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}" exec "$(command -v mariadb || command -v mysql)" -uroot -h127.0.0.1 -N -B' \
    <<<"CREATE DATABASE IF NOT EXISTS dockwell_proof; CREATE TABLE IF NOT EXISTS dockwell_proof.kept (v text); INSERT INTO dockwell_proof.kept VALUES ('$value');" ;;
  mysql/read) docker exec -i "$c" sh -c 'MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}" exec "$(command -v mariadb || command -v mysql)" -uroot -h127.0.0.1 -N -B' \
    <<<"SELECT v FROM dockwell_proof.kept;" ;;
  redis/write) docker exec "$c" sh -c 'REDISCLI_AUTH="${REDISCLI_AUTH:-${REDIS_PASSWORD:-}}" exec "$(command -v valkey-cli || command -v redis-cli)" "$@"' - SET dockwell_proof "$value" >/dev/null ;;
  redis/read) docker exec "$c" sh -c 'REDISCLI_AUTH="${REDISCLI_AUTH:-${REDIS_PASSWORD:-}}" exec "$(command -v valkey-cli || command -v redis-cli)" "$@"' - GET dockwell_proof ;;
  mongo/write) docker exec "$c" sh -c 'exec mongosh --quiet ${MONGO_INITDB_ROOT_USERNAME:+-u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin} --eval "$1"' - \
    "db.getSiblingDB('dockwell_proof').kept.insertOne({v: '$value'})" >/dev/null ;;
  mongo/read) docker exec "$c" sh -c 'exec mongosh --quiet ${MONGO_INITDB_ROOT_USERNAME:+-u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin} --eval "$1"' - \
    "print(db.getSiblingDB('dockwell_proof').kept.findOne().v)" ;;
  esac
}

# The harness's own probes, which judge every entry after its check, and are all an
# entry with no checks/<id>.sh of its own gets, the floor (ADR 0100).
#
# probe_data first writes a sentinel to each database the app keeps on a volume,
# with the engine's own client, before its check runs; probe_data upgraded holds
# every service that took one to running that engine, on a volume, and holding it,
# after the upgrade and its check: one gone, off its volume or another engine is data
# lost (#726's slice 2 review, M3).
probe_data() {
  local c service image engine value key held
  if [[ "$1" == first ]]; then
    for c in $(docker ps --filter "label=com.docker.compose.project=$project" --format '{{.Names}}'); do
      image=$(docker inspect --format '{{.Config.Image}}' "$c") || fail "docker inspect $c failed"
      engine=$(engine_of "$image") || continue
      # A database that keeps nothing, as a cache, has nothing to keep through an upgrade.
      [[ "$(docker inspect --format '{{len .Mounts}}' "$c")" != 0 ]] || continue
      service=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$c") || fail "docker inspect $c failed"
      value="kept-$id-$RANDOM$RANDOM"
      sentinel "$engine" "$c" write "$value" || fail "$service, $engine, won't take a sentinel through its own client"
      sentinels["$id/$service"]="$engine $value"
    done
    return
  fi
  for key in "${!sentinels[@]}"; do
    [[ "$key" == "$id/"* ]] || continue
    service=${key#"$id/"}
    read -r engine held <<<"${sentinels[$key]}"
    c=$(docker ps --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$service" --format '{{.Names}}') ||
      fail "docker ps failed"
    [[ -n "$c" && "$c" != *$'\n'* ]] || fail "$service, which held its data on a volume before the upgrade, doesn't run after it"
    image=$(docker inspect --format '{{.Config.Image}}' "$c") || fail "docker inspect $c failed"
    [[ "$(engine_of "$image" || true)" == "$engine" ]] || fail "$service ran $engine before the upgrade, and runs $image after it"
    [[ "$(docker inspect --format '{{len .Mounts}}' "$c")" != 0 ]] || fail "$service kept its data on a volume before the upgrade, and has none after it"
    value=$(sentinel "$engine" "$c" read) || fail "$service, $engine, won't read its sentinel back through its own client"
    [[ "$value" == *"$held"* ]] || fail "$service lost what it held through the upgrade: it reads '$value'"
  done
}
# probe_web says what the app opens answers: its entry's web service, on the port
# Dockwell opens it on. That's the one TCP port the service publishes; or, where it
# publishes several, the one its PORT input publishes, as an entry's web port is;
# or, with no PORT among them, any one that answers a page, as an SFTP or S3 port
# beside it doesn't. An entry with no web service has no page to probe.
probe_web() {
  local web ports port out
  web=$(jq -r '.web.service // empty' <<<"$entry")
  [[ -n "$web" ]] || return 0
  mapfile -t ports < <(docker inspect --format '{{json .HostConfig.PortBindings}}' "$(container "$web")" |
    jq -r '(. // {}) | to_entries[] | select(.key | endswith("/udp") | not) | .value[]?.HostPort | select(. != "")' | sort -u)
  ((${#ports[@]} > 0)) || fail "$web publishes no port"
  if ((${#ports[@]} > 1)); then
    port=$(dw catalog installs --json | jq -r --arg p "$installed" '.installs[] | select(.project == $p) | .pin.values.PORT // empty') ||
      fail "catalog installs failed"
    if [[ -n "$port" && " ${ports[*]} " == *" $port "* ]]; then ports=("$port"); fi
  fi
  for _ in $(seq 60); do
    for port in "${ports[@]}"; do
      out=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/" 2>/dev/null) || out=""
      [[ -n "$out" && "$out" != 000 && "$out" -lt 500 ]] && return 0
    done
    sleep 1
  done
  fail "$web answers no page on ${ports[*]} a minute after the change, want one"
}

# What a person types for an entry, as dockwell catalog install takes it.
declare -A given=()
# The companions an entry's check signs in through, which an install leaves out unless asked (ADR 0076).
declare -A companions=()
# Secret inputs use their names above and values in the CLI's environment, including under sudo.
declare -A given_env=()
# The earliest release a check says can run here at all, as a version: an earlier
# release of the catalog's isn't proven, as no MongoDB 8.0 before 8.0.38 starts on
# the kernels CI's runners run.
declare -A earliest=()
# The release before, as a check names it where the catalog's history has none of
# another version: its version, then each image by digest.
declare -A before=()
# Why an entry proves no upgrade, where its check says so, which --require-upgrade
# otherwise fails.
declare -A upgrade_waived=()
# The services an entry's check proves its far end refuses, as cloudflared's tunnel
# is refused on a token of no account: each runs with no health check, which it
# can't pass, and isn't held to running, as it stops and starts again on the
# refusal; the check proves the refusal instead (#745). Space-separated, as given.
# What a real value does is left unproven, so the entry is floored.
declare -A refused=()
# The services of the entry being proven that read a stand-in the floor gave for a
# secret, which run with no health check and are held to running instead (#745);
# what that leaves unproven, for the floored list; the change settled last held
# them for, so each change holds them once; and the services its check declares
# refused, which run with no health check and aren't held, and floor the entry.
unready="" floor_notes="" held_for="" refused_of=""

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
  # A service that reads a stand-in for a secret runs with no health check (#745).
  jq --argjson n "$number" '.release = $n' <<<"$entry" | drop_entry_health "$unready $refused_of" >"$catalog_dir/$id.yaml"
}

# The floor's stand-ins for an entry's required inputs, where it has no check to give
# them (#745): each input no person may leave empty, with no default and not a
# secret Dockwell generates, gets a value fitting its type and what the entry says
# it takes, which is no real credential and names no real host: an email and a host
# at .invalid, which never resolves, and for a secret a token in the shape apps such
# as cloudflared read, base64 of a JSON object of no account anywhere, or hex where
# the entry takes only what a URL carries or more than that holds. A stand-in may
# make an app dial its upstream's service, as cloudflared dials Cloudflare's edge,
# which refuses it. standins sets given_of and given_env_of; unready, the services
# that read a secret's stand-in; and floor_notes, what that leaves unproven.
standin_secret() {
  local characters=$1 least=$2 shape n
  shape=$(printf '{"a":"%s","t":"%s","s":"%s"}' "$(openssl rand -hex 16)" "$(cat /proc/sys/kernel/random/uuid)" "$(openssl rand -base64 32)" | base64 -w0)
  if [[ "$characters" != CHARACTERS_URL_SAFE ]] && ((${#shape} >= least)); then
    echo "$shape"
    return
  fi
  n=$((least > 48 ? least : 48))
  openssl rand -hex $(((n + 1) / 2)) | cut -c1-"$n"
}
# standin_text is a text input's stand-in, by its name, kept to the characters and
# the length the entry says it takes.
standin_text() {
  local name=$1 characters=$2 least=$3 value
  case "${name^^}" in
  *URL*) value="https://$id.invalid" ;;
  *DOMAIN* | *HOST*) value="$id.invalid" ;;
  *) value="catalog-proof" ;;
  esac
  [[ "$characters" != CHARACTERS_URL_SAFE ]] || value=$(tr -c 'A-Za-z0-9._~\n-' '-' <<<"${value#https://}")
  while ((${#value} < least)); do value+="-$(openssl rand -hex 4)"; done
  echo "$value"
}
# services_in prints the services of a Compose file, a line each.
services_in() {
  awk '
    function indent(s) { match(s, /^ */); return RLENGTH }
    /^ *services:[ \t]*$/ { base = indent($0); in_services = 1; next }
    in_services && $0 !~ /^[ \t]*$/ && indent($0) <= base { in_services = 0 }
    in_services && indent($0) == base + 2 && /^ *[A-Za-z0-9_.-]+:[ \t]*$/ { service = $1; sub(/:$/, "", service); print service }'
}
# reads prints the services of a Compose file that read one of the names given, by
# the whole name: ${NAME}, ${NAME:-...} and its kin, or $NAME, never ${NAME_TTL}.
reads() {
  awk -v names="$1" '
    BEGIN { n = split(names, want, " ") }
    function indent(s) { match(s, /^ */); return RLENGTH }
    /^ *services:[ \t]*$/ { base = indent($0); in_services = 1; next }
    in_services && $0 !~ /^[ \t]*$/ && indent($0) <= base { in_services = 0 }
    in_services && indent($0) == base + 2 && /^ *[A-Za-z0-9_.-]+:[ \t]*$/ { service = $1; sub(/:$/, "", service); next }
    in_services && service != "" {
      # A doubled dollar is the Compose way to write a literal one: not a read.
      line = $0; gsub(/\$\$/, "", line)
      for (i = 1; i <= n; i++)
        if (line ~ ("\\$\\{" want[i] "[}:?+-]") || line ~ ("\\$" want[i] "([^A-Za-z0-9_{]|$)")) print service
    }' | sort -u | tr '\n' ' '
}
# drop_health sets the health check of each service named to none, in a Compose file
# or an entry's YAML that holds one, which Dockwell then holds to keeping running
# rather than to a check its stand-in can't pass (its review, M1).
drop_health() {
  awk -v names="$1" '
    BEGIN { n = split(names, want, " "); for (i = 1; i <= n; i++) drop[want[i]] = 1 }
    function indent(s) { match(s, /^ */); return RLENGTH }
    skipping && ($0 ~ /^[ \t]*$/ || indent($0) > skip_at) { next }
    { skipping = 0 }
    /^ *services:[ \t]*$/ { base = indent($0); in_services = 1; print; next }
    in_services && $0 !~ /^[ \t]*$/ && indent($0) <= base { in_services = 0 }
    in_services && indent($0) == base + 2 && /^ *[A-Za-z0-9_.-]+:[ \t]*$/ {
      service = $1; sub(/:$/, "", service); print
      if (service in drop) printf "%" (base + 4) "s%s\n%" (base + 6) "s%s\n", "", "healthcheck:", "", "test: [\"NONE\"]"
      next
    }
    in_services && (service in drop) && indent($0) == base + 4 && /^ *healthcheck:/ { skipping = 1; skip_at = indent($0); next }
    { print }'
}
# drop_entry_health is drop_health on an entry's file, as YAML or JSON, whichever
# it is; nothing named, it's as it was.
drop_entry_health() {
  local text
  text=$(cat)
  if [[ -z "${1// /}" ]]; then
    printf '%s\n' "$text"
  elif jq -e . >/dev/null 2>&1 <<<"$text"; then
    jq --arg c "$(jq -r .compose <<<"$text" | drop_health "$1")"$'\n' '.compose = $c' <<<"$text"
  else
    printf '%s\n' "$text" | drop_health "$1"
  fi
}
standins() {
  local name type characters least value secrets=()
  while IFS=$'\t' read -r name type characters least; do
    case "$type" in
    INPUT_TYPE_EMAIL) given_of+=" $name=owner@$id.invalid" ;;
    INPUT_TYPE_PORT) given_of+=" $name=$((20000 + RANDOM % 10000))" ;;
    INPUT_TYPE_SECRET)
      value=$(standin_secret "$characters" "$least")
      given_of+=" $name" given_env_of+=" $name=$value"
      secrets+=("$name")
      ;;
    *) given_of+=" $name=$(standin_text "$name" "$characters" "$least")" ;;
    esac
    step "$id: has no check to give $name, so the floor gives it a stand-in"
  done < <(jq -r '.inputs[]? | select((.optional | not) and ((.defaultValue // "") == "")
    and ((.type == "INPUT_TYPE_SECRET" and ((.generate // 0) > 0)) | not))
    | [.name, (.type // "INPUT_TYPE_TEXT"), (.characters // "-"), (.minLength // 0)] | @tsv' <<<"$entry")
  ((${#secrets[@]} > 0)) || return 0
  unready=$(jq -r .compose <<<"$entry" | reads "${secrets[*]}")
  [[ -n "$unready" ]] || return 0
  unready=${unready% }
  floor_notes="${unready// /, } not held to a health check, as it reads a stand-in for ${secrets[*]}, which connects to nothing it would with a real one"
  step "$id: runs ${unready// /, } with no health check, as it reads a stand-in for ${secrets[*]}: the proof holds it to running for ${E2E_HOLD:-120} s with no restart, and keeping its data"
}

# settled waits for every container of the app to pass its health check, and says
# each runs an image pinned by digest.
settled() {
  local names health image service started running restarts was
  names=$(docker ps --all --filter "label=com.docker.compose.project=$project" --format '{{.Names}}') || fail "docker ps failed"
  [[ -n "$names" ]] || fail "$id runs nothing once its install has applied"
  for c in $names; do
    image=$(docker inspect --format '{{.Config.Image}}' "$c") || fail "docker inspect $c failed"
    [[ "$image" == *@sha256:* ]] || fail "$c runs $image, not pinned by digest"
    [[ -z "${1:-}" || "$1" == *"image: $image"* ]] || fail "$c runs $image, which its entry doesn't name"
    # A service that uses a stand-in the floor gave for a secret reaches nothing
    # with it, so it's held to having started, not to its health; and one its check
    # proves refused is held to that check (#745).
    service=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$c") || fail "docker inspect $c failed"
    if [[ " $unready $refused_of " == *" $service "* ]]; then
      continue
    fi
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$c") || fail "docker inspect $c failed"
    [[ "$health" == healthy ]] || fail "$c reads $health once the change has applied, want healthy: its entry needs a health check that proves it answers"
  done
  # A service that reads a stand-in for a secret runs with no health check, so it's
  # held to running, with no restart and the same start, for E2E_HOLD seconds, 120
  # unless set, past the 90 s cloudflared's own check allowed, after each change. An
  # exit fails it whatever its code, as a bad token's, a broken binary's and a wrong
  # platform's look alike (its review, M1; its re-check, N1).
  if [[ -n "$unready" && "${change:-}" != "$held_for" ]]; then
    held_for=${change:-}
    declare -A first=()
    for c in $names; do
      service=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$c") || fail "docker inspect $c failed"
      [[ " $unready " == *" $service "* ]] || continue
      first[$c]=$(docker inspect --format '{{.State.StartedAt}}' "$c") || fail "docker inspect $c failed"
    done
    sleep "${E2E_HOLD:-120}"
    for c in "${!first[@]}"; do
      service=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$c") || fail "docker inspect $c failed"
      running=$(docker inspect --format '{{.State.Running}}' "$c") || fail "docker inspect $c failed"
      restarts=$(docker inspect --format '{{.RestartCount}}' "$c") || fail "docker inspect $c failed"
      started=$(docker inspect --format '{{.State.StartedAt}}' "$c") || fail "docker inspect $c failed"
      was=${first[$c]}
      [[ "$running" == true && "$restarts" == 0 && "$started" == "$was" ]] ||
        fail "can't prove $service: it stops on a stand-in for a secret it reads (running $running, $restarts restarts, started $was then $started). Give checks/$id.sh, with a value it runs on"
    done
  fi
  # Every port of every service answers only on this server until it has a domain,
  # but one its entry declares public on purpose, as a proxy's 80 and 443, which the
  # install's plan had a person approve (ADR 0101); and what a person opens has one.
  local web bound public service installs values
  installs=$(dw catalog installs --json) || fail "catalog installs failed"
  values=$(jq -c --arg p "$installed" '[.installs[] | select(.project == $p) | .pin.values][0] // {}' <<<"$installs")
  # What the entry declares public, as "<service> <port>", from its file and the
  # install's values, so a port passes only on its own service and protocol.
  public=$(jq -n --argjson entry "$entry" --argjson values "$values" '{entry: $entry, values: $values}' | "$catalog_tool" public-ports | jq -Rsc 'split("\n") | map(select(. != ""))') ||
    fail "reading what $id declares public failed"
  for c in $names; do
    service=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$c") || fail "docker inspect $c failed"
    bound=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$c") || fail "docker inspect $c failed"
    jq -e --argjson public "$public" --arg service "$service" '
      [(. // {}) | to_entries[] | (.key | split("/")[1] // "tcp") as $protocol | .value[] | select(.HostIp != "127.0.0.1") | . + {protocol: $protocol}]
      | all(.HostIp as $ip | ($service + " " + .HostPort + (if .protocol == "tcp" then "" else "/" + .protocol end)) as $port
          | ($port | IN($public[])) and ($ip == "" or $ip == "0.0.0.0" or $ip == "::"))' <<<"$bound" >/dev/null ||
      fail "$c is published past this server, where its entry declares only $public public: $bound"
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
  # The owner's own inputs reach only its steps, never a container (ADR 0105), so
  # an app whose one secret is its owner's password holds none.
  [[ -n "$holds" ]] || [[ "$(jq '(.owner // {}) as $o | [.inputs[]? | select(.type == "INPUT_TYPE_SECRET")
    | select([.name] - [$o.emailInput, $o.userInput, $o.passwordInput] != [])] | length' <<<"$entry")" == 0 ]] || fail "$id uses none of its secrets"
}

# plan_install plans installing $id with its flags, and sets planned, change, installed
# (the install's project, which is <id>-2 once a rolled-back install has used <id>),
# environment and project (its Compose project) from the plan. Each plan goes on the
# list a secret mustn't be in, and its project on the list cleanup removes, a plan a
# registry failed included.
#
# What a check gives that the release being installed doesn't take is left out, as
# an earlier release has no input for its owner's password yet, which the release a
# check proves does (ADR 0105).
plan_install() {
  local takes flags=() i name
  takes=" $(dw catalog get "$id" --json | jq -r '[.entry.inputs[]?.name] | join(" ")') " || fail "the catalog has no $id"
  for ((i = 0; i < ${#install_flags[@]}; i += 2)); do
    name=${install_flags[i + 1]%%=*}
    [[ "${install_flags[i]}" != --values || "$takes" == *" $name "* ]] || continue
    flags+=("${install_flags[i]}" "${install_flags[i + 1]}")
  done
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

# prove installs, checks and removes $id from the entry in $catalog_dir; and, given
# an earlier release's commit and version, installs that first and upgrades it to
# the entry in one plan that backs its volumes up, keeping its data and its secrets.
prove() {
  local from=${1:-} was=${2:-}
  if [[ "$from" == check ]]; then
    # The release before its check names: the entry with that version and those
    # images, as the catalog's history doesn't hold it.
    step "$id: $was, the release before its check names, installs, answers and signs in"
    release "$entry" 1 "${@:2}"
  elif [[ -n "$from" ]]; then
    step "$id: $was installs, answers and signs in"
    # As the catalog had it when this run started, before any check ran.
    [[ -n "${earlier_text[$id@$from]:-}" ]] || fail "the catalog's $from has no entries/$id.yaml"
    printf '%s\n' "${earlier_text[$id@$from]}" | sed 's/^release: .*/release: 1/' | drop_entry_health "$unready $refused_of" >"$catalog_dir/$id.yaml"
  else
    step "$id: $version installs, answers and signs in"
    release "$entry" 1
  fi
  plans=()
  plan_install
  apply_patiently plan_install
  if [[ -n "$from" ]]; then
    settled
    probe_data first
    run_check first "$was"

    step "$id: upgrades from $was to $version in one plan, keeping its data and its secrets"
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
    secrets_after=$(held)
    [[ "$secrets_after" == "$secrets_before" ]] || fail "an upgrade of $id changed its secrets"
    settled "$(jq -r .compose <<<"$entry")"
    run_check upgraded "$version"
    # The harness's own probes, after the check: what passes the entry, whatever the
    # check's process did or said.
    settled "$(jq -r .compose <<<"$entry")"
    probe_data upgraded
  else
    settled "$(jq -r .compose <<<"$entry")"
    run_check first "$version"
    settled "$(jq -r .compose <<<"$entry")"
  fi
  probe_web
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
  # The next upgrade of it starts from nothing of this one's.
  docker volume ls --quiet --filter "label=com.docker.compose.project=$project" | xargs --no-run-if-empty docker volume rm --force >/dev/null 2>&1 || true
  project=""
}

# run_check runs a phase of the entry's check, with version the release it checks,
# as a process of its own: a fresh copy of this script, as it was when the run
# started, sources a fresh copy of the check, as the catalog had it then, so what one
# check's process does to its files or its shell reaches neither its own verdict nor
# the next entry (#726's slice 2 re-check, N3). It passes what the check planned back,
# for sealed to read, and fails the entry where the check does. An entry with no
# check has none to run.
run_check() {
  local phase=$1 dir plan
  [[ -n "${check_text[$id]+set}" ]] || return 0
  dir=$(mktemp -d "$work/check.XXXXXX")
  printf '%s\n' "$harness_text" >"$dir/catalog-e2e.sh"
  printf '%s\n' "${check_text[$id]}" >"$dir/check.sh"
  chmod 0444 "$dir/catalog-e2e.sh" "$dir/check.sh"
  chmod 0555 "$dir"
  : >"$state/plans"
  env E2E_ID="$id" E2E_VERSION="${2:-}" E2E_PROJECT="${project:-}" E2E_INSTALLED="${installed:-}" E2E_ENVIRONMENT="${environment:-}" E2E_ENTRY="${entry:-}" \
    E2E_BIN="$bin" E2E_ADDR="$addr" E2E_DATA="$data" E2E_LOG="$log" E2E_COOKIE="$cookie" E2E_PASSKEY="$passkey" E2E_STEPUP="$stepup" \
    E2E_CATALOG_DIR="$catalog_dir" E2E_MEDIA_DIR="$media_dir" E2E_STATE="$state" E2E_INPUT_FILE="${input_file:-}" \
    E2E_CATALOG_TOOL="$catalog_tool" \
    bash "$dir/catalog-e2e.sh" --check "$phase" "$dir/check.sh" || fail "check_$id $phase failed"
  while read -r plan; do
    [[ "$plan" =~ ^chg_[A-Za-z0-9]+$ ]] && plans+=("$plan")
  done <"$state/plans"
}

# A check's process: the harness's own functions can't be redefined by what the
# check defines, as a check that tries fails (#726's slice 2 review, M4). Its
# declarations go back as JSON; a phase runs check_<id>, and keeps kept and the plans
# it made in the entry's state.
if [[ -n "$check_phase" ]]; then
  # What a check and the harness's functions it calls read, from the run that judges
  # it, read-only: the entry and its install, as the run has them now, and Dockwell's
  # CLI, its session and passkey, and the folders it reads from (#743's first run).
  readonly id=$E2E_ID version=$E2E_VERSION project=$E2E_PROJECT installed=$E2E_INSTALLED environment=$E2E_ENVIRONMENT entry=$E2E_ENTRY \
    bin=$E2E_BIN addr=$E2E_ADDR data=$E2E_DATA log=$E2E_LOG cookie=$E2E_COOKIE passkey=$E2E_PASSKEY stepup=$E2E_STEPUP \
    catalog_dir=$E2E_CATALOG_DIR media_dir=$E2E_MEDIA_DIR state=$E2E_STATE dockwell_port=${E2E_ADDR##*:} \
    catalog_tool=$E2E_CATALOG_TOOL
  readonly input_file=${E2E_INPUT_FILE:-}
  mapfile -t harness < <(declare -F | awk '{print $3}')
  readonly -f "${harness[@]}"
  if [[ -f "$state/kept" ]]; then
    # shellcheck source=/dev/null
    source "$state/kept"
  fi
  # shellcheck source=/dev/null
  source "$check_file"
  if [[ "$check_phase" == declare ]]; then
    defined=false
    ! declare -F "check_$id" >/dev/null || defined=true
    jq -n --argjson defined "$defined" --arg given "${given[$id]:-}" --arg given_env "${given_env[$id]:-}" --arg companions "${companions[$id]:-}" \
      --arg earliest "${earliest[$id]:-}" --arg before "${before[$id]:-}" --arg upgrade_waived "${upgrade_waived[$id]:-}" --arg refused "${refused[$id]:-}" \
      '{$defined, $given, $given_env, $companions, $earliest, $before, $upgrade_waived, $refused}' >"$state/declared.json"
    exit 0
  fi
  declare -F "check_$id" >/dev/null || fail "checks/$id.sh defines no check_$id"
  "check_$id" "$check_phase"
  declare -p kept >"$state/kept"
  ((${#plans[@]} == 0)) || printf '%s\n' "${plans[@]}" >"$state/plans"
  exit 0
fi

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
(cd "$dockwell" && go run ./scripts/claim "$link" "$cookie" "$passkey" && go build -o "$stepup" ./scripts/stepup && go build -o "$catalog_tool" ./cmd/dockwell-catalog) || fail "can't claim the install"
dw login --local >/dev/null || fail "dockwell login --local failed"

# Everything the run reads of the catalog, before any check runs: each entry, its
# check, and its earlier releases, so a check that writes to the checkout reaches no
# later entry. The lists of entries proven with the floor alone, or with no upgrade,
# stay in this process, and go to E2E_FLOORED and E2E_NOT_UPGRADED at the end.
declare -A entry_text=() check_text=() earlier_of=() earlier_text=()
harness_text=$(<"$copy/catalog-e2e.sh")
for id in "$@"; do
  [[ "$id" =~ ^[a-z0-9-]+$ ]] || fail "$id isn't an entry's ID"
  [[ -f "$catalog/entries/$id.yaml" ]] || fail "the catalog has no entries/$id.yaml"
  entry_text[$id]=$(<"$catalog/entries/$id.yaml")
  if [[ -f "$catalog/checks/$id.sh" ]]; then
    check_text[$id]=$(<"$catalog/checks/$id.sh")
  elif ((no_floor)); then
    fail "the catalog has no checks/$id.sh, and this run proves no entry with the floor alone. Write one"
  fi
  # From the release before, as the pull request's base has it, and from the first
  # release of the current major, as an install that waited months would; from
  # nothing for an entry that's new.
  earlier_of[$id]=$("$catalog_tool" earlier --catalog "$catalog" --at "$at" ${base:+--base "$base"} "$id") || fail "reading $id's earlier releases failed"
  for which in before first; do
    from=$(jq -r --arg w "$which" '.[$w].commit // empty' <<<"${earlier_of[$id]}")
    # By the entry and the commit, as one commit can hold the release before of several.
    [[ -z "$from" ]] || earlier_text[$id@$from]=$(git -C "$catalog" show "$from:entries/$id.yaml") || fail "the catalog's $from has no entries/$id.yaml"
  done
done
floored=() not_upgraded=()
declare -A floored_notes=()

for id in "$@"; do
  started=$SECONDS
  state=$(mktemp -d "$work/state.XXXXXX")
  given_of="" given_env_of="" companions_of="" earliest_of="" before_of="" waived_of="" unready="" floor_notes="" held_for="" refused_of="" input_file=""
  # Its own check where the catalog has one, else the floor, with which it doesn't
  # merge by itself (ADR 0100).
  if [[ -n "${check_text[$id]+set}" ]]; then
    run_check declare
    declared=$(<"$state/declared.json")
    jq -e .defined <<<"$declared" >/dev/null || fail "checks/$id.sh defines no check_$id"
    given_of=$(jq -r .given <<<"$declared")
    given_env_of=$(jq -r .given_env <<<"$declared")
    companions_of=$(jq -r .companions <<<"$declared")
    earliest_of=$(jq -r .earliest <<<"$declared")
    before_of=$(jq -r .before <<<"$declared")
    waived_of=$(jq -r .upgrade_waived <<<"$declared")
    read -r -a named <<<"$(jq -r .refused <<<"$declared")"
    refused_of=${named[*]}
  else
    step "$id: has no checks/$id.sh, so it gets the floor alone"
    floored+=("$id")
  fi
  printf '%s\n' "${entry_text[$id]}" >"$catalog_dir/$id.yaml"
  entry=$(dw catalog get "$id" --json | jq .entry) || fail "the catalog has no $id"
  [[ "$entry" != null ]] || fail "the catalog has no $id"
  version=$(jq -r .version <<<"$entry")
  # With no check to give what a person types, the floor gives stand-ins (#745).
  [[ -n "${check_text[$id]+set}" ]] || standins
  # A service its check proves refused, by its far end, is one the entry runs. Its
  # entry is floored, though its check runs: what the service does with a real value
  # is left unproven, so it doesn't merge by itself.
  if [[ -n "$refused_of" ]]; then
    runs=" $(jq -r .compose <<<"$entry" | services_in | tr '\n' ' ')"
    for s in $refused_of; do
      [[ "$runs" == *" $s "* ]] || fail "checks/$id.sh says its far end refuses $s, and $id runs no $s. Name a service of its compose in refused[$id]"
    done
    step "$id: runs ${refused_of// /, } with no health check and doesn't hold it to running, as its check proves its far end refuses it: what it does with a real value is left unproven, so it's floored"
    floored+=("$id")
    floor_notes="${refused_of// /, }'s far end refuses it, so what it does with a real value is left unproven"
  fi
  [[ -z "$floor_notes" ]] || floored_notes[$id]=$floor_notes
  read -r -a values <<<"$given_of"
  inputs_file
  install_flags=()
  for v in "${values[@]}"; do install_flags+=(--values "$v"); done
  # What the entry opens may be a companion an install leaves out unless asked, as
  # mysql's phpMyAdmin is: the proof turns it on, so what it opens runs and is
  # proven, its ports with it (ADR 0076, ADR 0101; catalog#31).
  opens=$(jq -r '.web.service // empty' <<<"$entry")
  if [[ -n "$opens" && " $companions_of " != *" $opens "* ]] && jq -e --arg w "$opens" 'any(.companions[]?; .service == $w)' <<<"$entry" >/dev/null; then
    companions_of="$companions_of $opens"
  fi
  for c in $companions_of; do install_flags+=(--companions "$c=true"); done
  earlier=${earlier_of[$id]}
  proved=0 skipped=0
  history_before=$(jq -r '.before.version // empty' <<<"$earlier")
  # A release before its check names, by its version and image digests, where the
  # history holds none of another version, as a young catalog's doesn't.
  if [[ -n "$before_of" ]]; then
    read -r -a named <<<"$before_of"
    if [[ "${named[0]}" != "$version" && "${named[0]}" != "$history_before" ]]; then
      prove check "${named[@]}"
      proved=1
    fi
  fi
  for which in before first; do
    from=$(jq -r --arg w "$which" '.[$w].commit // empty' <<<"$earlier")
    [[ -n "$from" ]] || continue
    was=$(jq -r --arg w "$which" '.[$w].version' <<<"$earlier")
    if [[ -n "$earliest_of" && "$(printf '%s\n' "$was" "$earliest_of" | sort -V | head -1)" != "$earliest_of" ]]; then
      step "$id: $was is before $earliest_of, the earliest its check says runs here, so it isn't proven from"
      skipped=1
      continue
    fi
    prove "$from" "$was"
    proved=1
  done
  if ((!proved)) && ((require_upgrade)); then
    # Dockwell's CI proves an upgrade of every entry, or says in the check why not.
    [[ -n "$waived_of" ]] ||
      fail "$id has no earlier release to upgrade from. Give checks/$id.sh a before[$id] line: the release before, by its version and each image by digest"
    step "$id: proves no upgrade, as its check says: $waived_of"
  fi
  if ((!proved)) && ((skipped)); then
    # Upgraded from nothing, as every earlier release was before its earliest: not a
    # full proof, which slice 3 leaves to a person, as with the floor.
    not_upgraded+=("$id")
  fi
  # An entry whose owner Dockwell makes is installed fresh as well, as an upgrade
  # makes no owner for an install from before, so its steps run here too (ADR 0105).
  if ((!proved)) || jq -e '(.owner.steps // []) | length > 0' <<<"$entry" >/dev/null; then
    prove
  fi
  rm -f "${catalog_dir:?}/${id:?}.yaml"
  # Its secrets' values go with the entry; cleanup takes the folder they were in.
  if [[ -n "$input_file" ]]; then rm -f -- "$input_file"; fi
  input_file=""
  echo "$id took $((SECONDS - started)) s"
done

# Written once, at the end, from this process's own lists, over anything a check
# wrote there.
if [[ -n "${E2E_FLOORED:-}" ]]; then
  : >"$E2E_FLOORED"
  for id in "${floored[@]}"; do
    # What a stand-in, or a refusal its check proves, left unproven, beside the entry,
    # for the person who merges.
    if [[ -n "${floored_notes[$id]:-}" ]]; then echo "$id: ${floored_notes[$id]}"; else echo "$id"; fi
  done >"$E2E_FLOORED"
fi
if [[ -n "${E2E_NOT_UPGRADED:-}" ]]; then
  : >"$E2E_NOT_UPGRADED"
  ((${#not_upgraded[@]} == 0)) || printf '%s\n' "${not_upgraded[@]}" >"$E2E_NOT_UPGRADED"
fi
echo "catalog-e2e: ok"
