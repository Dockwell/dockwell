#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Runs built dockwell binaries against this machine's Docker Engine, end to
# end through the CLI: a service from an image runs, changes and is undone;
# the environment exports as a Compose file that docker compose reads and
# lists; a Compose file imports, and two with a service of one name run side
# by side; a service using a variable and a secret runs with their values,
# exports without the secret, and imports again with no change; a tag that moves
# in a local registry is pulled again, rolled back by digest, and survives that
# registry going away; and everything goes again. A file in the shapes real ones
# have runs on a network the server has, with what it asks for really applied,
# and reaches another project on it by name. Then the agent build joins the
# full build's control plane, and runs a container on the same Docker. Before
# that, a route gets a real certificate from Pebble, Let's Encrypt's test CA,
# and serves a real container over HTTPS through Caddy, and routes that take no
# certificate serve over plain HTTP on port 80 alone, to what they name. It uses throwaway data
# directories and removes what it made, so it never touches a real install. It
# needs Docker with the compose plugin, Go, curl, jq, openssl and iproute2's ss,
# a free port (E2E_WEB_PORT, 18080 by default), and ports 80 and 443 free, for
# the ingress's own part. With E2E_PANEL set, as in CI, it also moves the panel's
# own domain behind Caddy and back, which needs sudo to let this user bind ports
# 80 and 443. With E2E_ROOT set, as in CI, Dockwell runs as root through sudo, as
# it does on a server, so Caddy runs as its own user and its directories are given
# to it, and it restarts Docker itself along the way.
#
#   scripts/docker-e2e.sh bin/dockwell bin/dockwell-agent
set -euo pipefail

usage="usage: scripts/docker-e2e.sh path/to/dockwell path/to/dockwell-agent"
for tool in docker curl go jq openssl ss; do
  command -v "$tool" >/dev/null || {
    echo "docker-e2e: $tool isn't installed, and the script needs it. Install it, such as with apt-get install $tool, then run it again" >&2
    exit 1
  }
done
bin=$(realpath "${1:?$usage}")
agent=$(realpath "${2:?$usage}")
addr=127.0.0.1:${E2E_PORT:-7697}
web_port=${E2E_WEB_PORT:-18080}
data=$(mktemp -d)
agent_data=$(mktemp -d)
log=$(mktemp)
agent_log=$(mktemp)
project=""
extra_projects=""
real_projects=()
agent_pid=""
moving_dir=""
moving_repo=""
digest_first=""
digest_second=""
digest_third=""
# A registry, by digest, for the images of the step about a tag that moves.
registry_image=registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373

port=${addr##*:}
cookie=$(mktemp)
# What a file's relative paths start from. Not the data directory, which no
# container may mount, and Docker makes what's in it as root.
real_dir=$(mktemp -d)

pebble_dir=$(mktemp -d)
tools=${E2E_INGRESS_TOOLS:-$(mktemp -d)}
pebble_pids=()

# as_dockwell is what runs a command as whoever Dockwell runs as, and priv does so
# for what touches its files. A function run in the background is a subshell, whose
# PID isn't the command's, so start_dockwell uses the array.
as_dockwell=()
[[ -z "${E2E_ROOT:-}" ]] || as_dockwell=(sudo -n)
priv() { "${as_dockwell[@]}" "$@"; }

# The ingress's certificates come from Pebble, on this machine.
export DOCKWELL_ACME_DIRECTORY_URL=https://localhost:14000/dir
export DOCKWELL_ACME_ROOTS=$pebble_dir/cert.pem
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 -subj /CN=localhost \
  -addext subjectAltName=DNS:localhost,IP:127.0.0.1 -keyout "$pebble_dir/key.pem" -out "$DOCKWELL_ACME_ROOTS" 2>/dev/null

start_dockwell() {
  # SSL_CERT_FILE is for the panel's own certificates alone, as they come through
  # Go's default roots. sudo keeps none of these unless it is told.
  "${as_dockwell[@]}" env SSL_CERT_FILE="$DOCKWELL_ACME_ROOTS" DOCKWELL_ACME_DIRECTORY_URL="$DOCKWELL_ACME_DIRECTORY_URL" DOCKWELL_ACME_ROOTS="$DOCKWELL_ACME_ROOTS" \
    "$bin" run --listen "$addr" --agent-listen 127.0.0.1:0 --data-dir "$data" --log-format json "$@" >>"$log" 2>&1 &
  pid=$!
}
start_dockwell
# moving_tidy removes what the step about a tag that moves brought here.
moving_tidy() {
  docker rm --force --volumes e2e-registry >/dev/null 2>&1 || true
  docker rmi e2e-moving:first e2e-moving:second e2e-moving:third "$moving_repo:latest" "$moving_repo@$digest_first" "$moving_repo@$digest_second" "$moving_repo@$digest_third" >/dev/null 2>&1 || true
  [[ -z "$moving_dir" ]] || rm -rf "$moving_dir"
  moving_dir=""
}
cleanup() {
  for p in $project $extra_projects; do
    docker compose -p "$p" down --volumes --remove-orphans --timeout 1 >/dev/null 2>&1 || true
  done
  for p in "${real_projects[@]}"; do
    docker compose -p "$p" down --volumes --remove-orphans --timeout 1 >/dev/null 2>&1 || true
  done
  docker image rm e2e-local-only:1 >/dev/null 2>&1 || true
  docker rm --force e2e-agent-web e2e-holds-80 e2e-returns dockwell-ingress dockwell-ingress-keep >/dev/null 2>&1 || true
  moving_tidy
  docker network rm dockwell_ingress e2e-took-subnet e2e-edge >/dev/null 2>&1 || true
  [[ -z "$agent_pid" ]] || kill "$agent_pid" 2>/dev/null || true
  priv kill "$pid" 2>/dev/null || true
  for p in "${pebble_pids[@]:-}"; do
    [[ -z "$p" ]] || kill "$p" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  priv rm -rf "$log" "$agent_log" "$cookie" "$data" "$agent_data" "$pebble_dir" "$real_dir"
}
trap cleanup EXIT

# ingress_state says all there is to know of the ingress, as most of what can
# go wrong with it is on real Docker, where nobody can run it but CI.
ingress_state() {
  echo "--- the ingress's container" >&2
  docker ps -a --filter name=dockwell-ingress --format '{{.Names}} {{.Image}} {{.Status}} {{.Ports}}' >&2 || true
  docker inspect --format 'network {{.HostConfig.NetworkMode}}, user {{.Config.User}}, restart {{.HostConfig.RestartPolicy.Name}}, exit {{.State.ExitCode}}, error {{.State.Error}}' dockwell-ingress >&2 2>/dev/null || true
  docker ps -a --filter name=dockwell-ingress-keep --format '{{.Names}} {{.Image}} {{.Status}}' >&2 || true
  echo "--- its log" >&2
  docker logs --tail 60 dockwell-ingress >&2 2>&1 || true
  echo "--- the ingress network" >&2
  docker network inspect --format '{{.Name}} internal={{.Internal}} {{json .IPAM.Config}} {{json .Containers}} {{json .Options}}' dockwell_ingress >&2 2>/dev/null || true
  echo "--- what certificates are doing" >&2
  priv tail -n 40 "$data/ingress/data/dockwell-tls.log" >&2 2>/dev/null || true
  echo "--- addresses" >&2
  priv cat "$data/ingress/addresses.json" >&2 2>/dev/null || true
  echo "--- who is on ports 80 and 443" >&2
  priv ss -ltnup 'sport = :80 or sport = :443' >&2 2>/dev/null || true
  echo "--- domains, as the API has them" >&2
  dw domains list --json >&2 2>/dev/null || true
  echo "--- Pebble" >&2
  tail -n 30 "$pebble_dir/pebble.out" >&2 2>/dev/null || true
}
in_ingress=""

fail() {
  echo "docker-e2e: $*" >&2
  [[ -z "$in_ingress" ]] || ingress_state
  echo "--- dockwell log" >&2
  cat "$log" >&2
  if [[ -s "$agent_log" ]]; then
    echo "--- the agent build's log" >&2
    cat "$agent_log" >&2
  fi
  exit 1
}

# Local access on the control server: the CLI finds the control plane, and
# makes its own token, from the data directory.
dw() { priv env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text "$bin" "$@"; }
step() { echo "--- $*"; }
# says PATTERN COMMAND... reports a command that ran, and printed something that
# holds PATTERN. It captures what it printed first, as grep -q stops reading at its
# first match: under pipefail what still writes to it can fail on the closed pipe,
# so a check that passed reads as failed, and in an if, one that should fail passes.
says() {
  local out
  out=$("${@:2}") || return 1
  grep -qE -- "$1" <<<"$out"
}

for _ in $(seq 100); do
  curl -fsS "http://$addr/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
[[ "$(curl -fsS "http://$addr/healthz")" == "ok" ]] || fail "/healthz did not answer ok within 10 s"
for _ in $(seq 300); do
  grep -q 'connected to the control plane' "$log" && break
  sleep 0.1
done
grep -q 'connected to the control plane' "$log" || fail "the control server's own agent didn't connect within 30 s"
if grep -q "the agent can't run some kinds" "$log"; then
  fail "the agent can't run the Docker kinds; is Docker running?"
fi

# Local access acts for the install's owner, so claim it first, with a
# software passkey, as the panel would. Local access is read-only until
# dockwell login --local lets it make changes.
link=$(grep -o 'http://localhost:[0-9]*/claim#[A-Za-z0-9_-]*' "$log" | head -1) || true
[[ -n "$link" ]] || fail "dockwell run printed no claim link"
(cd "$(dirname "$0")/.." && go run ./scripts/claim "$link" "$cookie") || fail "can't claim the install"
dw login --local >/dev/null || fail "dockwell login --local failed"

# person_call calls the API as a signed-in person does, from the panel's origin,
# for what no token may do.
person_call() {
  curl -fsS --max-time 30 --resolve "localhost:$port:127.0.0.1" -H "Origin: http://localhost:$port" -H 'Content-Type: application/json' \
    -H "Cookie: $(cat "$cookie")" -d "$2" "http://localhost:$port/dockwell.v1.$1"
}
# change_field reads a field of a change.
change_field() { dw changes get "$1" --json | jq -r ".change.$2"; }
# person_apply applies a planned change as a person does in the panel, and waits
# for it.
person_apply() {
  local fingerprint state=""
  fingerprint=$(change_field "$1" plan.fingerprint) || fail "can't read the plan of $1"
  person_call ChangeService/ApplyChange "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\"}" >/dev/null || fail "a person can't apply $1"
  for _ in $(seq 240); do
    state=$(change_field "$1" state) || state=""
    [[ "$state" == CHANGE_STATE_APPLIED ]] && return 0
    [[ "$state" == CHANGE_STATE_FAILED ]] && fail "$1 failed: $(dw changes get "$1" --json)"
    sleep 0.5
  done
  fail "$1 is $state after 120 s, want applied"
}
# token_refused checks that the server itself refuses a token applying a change,
# through its API, rather than the CLI's own check that stops short of asking. The
# token is the one that check protects: the read-write token dockwell login --local
# keeps, an API token like any other to the server. Each dw --yes before this has
# applied a plan with it, so a refusal is about this plan, and not the token.
token_refused() {
  local token fingerprint answer
  token=$(priv cat "$data/control/local-token.json" | jq -r .token) || fail "can't read the token dockwell login --local keeps"
  [[ "$token" == dwt_* ]] || fail "the token dockwell login --local keeps isn't an API token: ${token:0:8}"
  fingerprint=$(change_field "$1" plan.fingerprint) || fail "can't read the plan of $1"
  answer=$(curl -sS --max-time 30 -i -w '\n%{http_code}' -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -d "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\"}" "http://$addr/dockwell.v1.ChangeService/ApplyChange") || fail "the API didn't answer a token applying $1"
  [[ "${answer##*$'\n'}" == 403 ]] || fail "the server let a token apply $1: $answer"
  grep -qi '^dockwell-error: needs_session' <<<"$answer" || fail "the server refused a token applying $1 for another reason: $answer"
  [[ "$(change_field "$1" state)" == CHANGE_STATE_PLANNED ]] || fail "$1 isn't planned after the token was refused"
}

ports="{\"hostPort\":$web_port,\"containerPort\":80,\"hostIp\":\"127.0.0.1\"}"

step "A service from an image runs"
dw services create web --app-image nginx:1.27-alpine --app-ports "$ports" --app-env GREETING=hello --yes ||
  fail "services create failed"
container=$(docker ps --filter label=com.docker.compose.service=web --format '{{.Names}}') || fail "docker ps failed"
[[ "$container" == env_*-web-1 ]] || fail "Docker runs '$container' for web, want env_…-web-1"
project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$container") ||
  fail "docker inspect $container failed"
[[ "$project" == env_* ]] || fail "$container is in the Compose project '$project', want its environment's, env_…"
says nginx curl -fsS "http://127.0.0.1:$web_port/" || fail "web doesn't serve on port $web_port"
env_of() { docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container"; }
says '^GREETING=hello$' env_of || fail "web's environment is $(env_of)"

step "It changes, and the change undoes"
dw services update web --app-image nginx:1.27-alpine --app-ports "$ports" --app-env GREETING=hi --yes ||
  fail "services update failed"
says '^GREETING=hi$' env_of || fail "after the update, web's environment is $(env_of)"
# Changes are listed newest first.
changes=$(dw changes list --json) || fail "changes list failed"
change=$(jq -r '.changes[0].id // empty' <<<"$changes") || fail "changes list printed what isn't JSON: $changes"
[[ "$change" == chg_* ]] || fail "changes list has no change: $changes"
dw changes undo "$change" --yes || fail "changes undo $change failed"
says '^GREETING=hello$' env_of || fail "after the undo, web's environment is $(env_of)"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after the undo: $drift"

step "The environment exports as Compose, and docker compose lists it"
dw compose export >"$data/compose.yaml" || fail "compose export failed"
grep -q "^name: $project" "$data/compose.yaml" || fail "the export isn't project $project:
$(cat "$data/compose.yaml")"
if grep -q hello "$data/compose.yaml"; then fail "the export holds an environment variable's value"; fi
GREETING=hello docker compose -f "$data/compose.yaml" config --quiet || fail "docker compose doesn't take the export"
listed=$(GREETING=hello docker compose -f "$data/compose.yaml" ps --format '{{.Service}}') || fail "docker compose -f <export> ps failed"
[[ "$listed" == web ]] || fail "docker compose -f <export> ps lists '$listed', want web"
listed=$(docker compose -p "$project" ps --format '{{.Service}}') || fail "docker compose -p $project ps failed"
[[ "$listed" == web ]] || fail "docker compose -p $project ps lists '$listed', want web"

step "A Compose file imports"
cat >"$data/stack.yaml" <<'EOF'
services:
  cache:
    image: redis:7.4-alpine
    volumes: [cache-data:/data]
    networks: [back]
volumes:
  cache-data: {}
networks:
  back: {}
EOF
# A file made to cost too much is stopped by the limited child that reads it
# first, and the control plane carries on. One long word through a variable
# goes through a quadratic splitter, for far longer than a child is allowed.
long=$(head -c 120000 /dev/zero | tr '\0' x)
# shellcheck disable=SC2016 # ${A} is Compose's, for dockwell to read.
costly_file=$(printf 'services:\n  slow:\n    image: nginx:1.27-alpine\n    command: ${A}${A}${A}${A}\n')
if costly=$(dw compose import --compose "$costly_file" --variables "A=$long" --yes 2>&1); then
  fail "a file made to cost too much was read: ${costly:0:300}"
fi
[[ "$costly" == *"more time or memory"* ]] || fail "a file made to cost too much failed some other way: ${costly:0:300}"
dw changes list --json >/dev/null || fail "the control plane didn't carry on after a file that cost too much"

dw compose import --project default --compose "$(cat "$data/stack.yaml")" --yes || fail "compose import failed"
services=$(docker compose -p "$project" ps --format '{{.Service}}' | sort | tr '\n' ' ') || fail "docker compose -p $project ps failed"
[[ "$services" == "cache web " ]] || fail "after the import, docker compose lists '$services', want cache and web"
docker volume inspect "${project}_cache-data" >/dev/null || fail "the import made no volume ${project}_cache-data"

step "A service using a variable and a secret runs with their values"
secret_value=e2e-s3cret-value
# The secret's value comes from a file: the command line would put it in the
# process list, and the CLI takes it no other way.
printf '%s' "$secret_value" >"$data/secret-value"
dw variables create LOG_LEVEL --value debug --yes || fail "variables create failed"
dw secrets create DB_PASSWORD --value-file "$data/secret-value" --yes || fail "secrets create failed"
dw services create api --app-image nginx:1.27-alpine --variables LOG_LEVEL= --secrets DATABASE_PASSWORD=DB_PASSWORD --yes ||
  fail "services create api failed"
api=$(docker ps --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=api --format '{{.Names}}') ||
  fail "docker ps failed"
[[ "$api" == "$project-api-1" ]] || fail "Docker runs '$api' for api, want $project-api-1"
api_env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$api") || fail "docker inspect $api failed"
grep -qx "DATABASE_PASSWORD=$secret_value" <<<"$api_env" || fail "api doesn't run with the secret's value"
grep -qx LOG_LEVEL=debug <<<"$api_env" || fail "api doesn't run with the variable's value"
api_id=$(docker inspect --format '{{.Id}}' "$api") || fail "docker inspect $api failed"
api_labels=$(docker inspect --format '{{json .Config.Labels}}' "$api") || fail "docker inspect $api failed"
if [[ "$api_labels" == *"$secret_value"* ]]; then fail "api's labels hold the secret's value"; fi
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift with api running: $drift"

step "The environment exports without the secret, and docker compose lists it"
dw compose export >"$data/values.yaml" || fail "compose export failed"
if grep -q -e "$secret_value" "$data/values.yaml"; then fail "the export holds the secret's value"; fi
grep -Eq '^ +DATABASE_PASSWORD: null$' "$data/values.yaml" || fail "the export doesn't name DATABASE_PASSWORD without a value:
$(cat "$data/values.yaml")"
grep -Eq '^ +LOG_LEVEL: debug$' "$data/values.yaml" || fail "the export doesn't write LOG_LEVEL's value:
$(cat "$data/values.yaml")"
docker compose -f "$data/values.yaml" config --quiet || fail "docker compose doesn't take the export"
services=$(docker compose -f "$data/values.yaml" ps --format '{{.Service}}' | sort | tr '\n' ' ') || fail "docker compose -f <export> ps failed"
[[ "$services" == "api cache web " ]] || fail "docker compose -f <export> ps lists '$services', want api, cache and web"

step "Importing it again plans no change, and restarts nothing"
# web's own environment is by name in the file, so it comes as a variable, as
# any value of a container's own does. api's comes from what it uses. With
# --remove-orphans, anything the import lost would show as going. The CLI
# discards a plan with nothing in it, and says so on standard error.
imported=$(dw compose import --compose "$(cat "$data/values.yaml")" --variables GREETING=hello --remove-orphans --json 2>"$data/import.err") ||
  fail "compose import of the export failed: $(cat "$data/import.err")"
grep -q 'Nothing to change' "$data/import.err" || fail "compose import of the export didn't say there was nothing to change: $(cat "$data/import.err")"
# Each check says something an empty answer can't pass: a file that imported
# nothing has no api to find, and no plan has no summary to add up.
jq -e '.change.id | startswith("chg_")' <<<"$imported" >/dev/null || fail "compose import printed no change: $imported"
jq -e '[.change.plan.resources[] | select(.resource.kind == "service") | .resource.name] | sort == ["api", "cache", "web"]' <<<"$imported" >/dev/null ||
  fail "the import didn't plan api, cache and web: $imported"
jq -e '.change.plan | has("fingerprint")' <<<"$imported" >/dev/null || fail "the import has no plan: $imported"
jq -e '.change.plan.summary // {} | [.create, .change, .replace, .destroy] | map(. // 0) | add == 0' <<<"$imported" >/dev/null ||
  fail "importing the export plans a change: $imported"
jq -e '[.change.plan.resources[] | select((.changes // []) | length > 0)] | length == 0' <<<"$imported" >/dev/null ||
  fail "importing the export changes a field: $imported"
jq -e '[.change.plan.servers[]?.steps[]? | select((.action // "") | IN("", "PLAN_ACTION_NONE") | not)] | length == 0' <<<"$imported" >/dev/null ||
  fail "importing the export runs a step: $imported"
if [[ "$imported" == *"$secret_value"* ]]; then fail "the plan holds the secret's value"; fi
[[ "$(docker inspect --format '{{.Id}}' "$api")" == "$api_id" ]] || fail "importing the export recreated api"
api_env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$api") || fail "docker inspect $api failed"
grep -qx "DATABASE_PASSWORD=$secret_value" <<<"$api_env" || fail "importing the export took the secret's value from api"

step "A container docker compose makes from the export reads back as the service"
DATABASE_PASSWORD=$secret_value docker compose -f "$data/values.yaml" up --detach --no-deps api || fail "docker compose up api failed"
[[ "$(docker inspect --format '{{.Id}}' "$api")" != "$api_id" ]] || fail "docker compose up didn't recreate api, so this reads back nothing new"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after docker compose up from the export: $drift"

step "A rotation still reaches it, so the import kept the reference"
secret_next=e2e-s3cret-next
printf '%s' "$secret_next" >"$data/secret-value"
dw secrets update DB_PASSWORD --value-file "$data/secret-value" --yes || fail "secrets update failed"
api_env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$api") || fail "docker inspect $api failed"
grep -qx "DATABASE_PASSWORD=$secret_next" <<<"$api_env" || fail "the rotation didn't reach api"

dw services delete api --yes || fail "services delete api failed"
dw secrets delete DB_PASSWORD --yes || fail "secrets delete failed"
dw variables delete LOG_LEVEL --yes || fail "variables delete failed"
# Nothing above left a value where anyone reads it.
if grep -q -e "$secret_value" -e "$secret_next" "$log"; then fail "the control plane's log holds the secret's value"; fi

step "Services go"
dw services delete cache --yes || fail "services delete cache failed"
dw services delete web --yes || fail "services delete web failed"
left=$(docker compose -p "$project" ps -q) || fail "docker compose -p $project ps failed"
[[ -z "$left" ]] || fail "containers are left after deleting the services: $left"

# Deleting the volume destroys data, so only a person may apply it; the
# CLI says so and leaves it planned, with exit status 4.
set +e
dw volumes delete cache-data --yes >/dev/null 2>&1
status=$?
set -e
[[ "$status" == 4 ]] || fail "volumes delete exited $status, want 4: a plan that destroys data waits for a person"

stop_dockwell() {
  priv kill "$pid"
  wait "$pid" 2>/dev/null || true
  # Everything that says "with Dockwell down" depends on this.
  if curl -fsS --max-time 2 "http://$addr/healthz" >/dev/null 2>&1; then fail "Dockwell still answers after it was stopped"; fi
  connected=$(grep -c 'connected to the control plane' "$log" || true)
}
# wait_for_dockwell waits for what stop_dockwell stopped to answer, and for its
# agent to connect, which every plan needs.
wait_for_dockwell() {
  for _ in $(seq 300); do
    curl -fsS "http://$addr/healthz" >/dev/null 2>&1 &&
      [[ "$(grep -c 'connected to the control plane' "$log")" -gt "$connected" ]] && return 0
    sleep 0.1
  done
  fail "Dockwell didn't come back within 30 s"
}
# kill_dockwell stops it with no warning, in the middle of whatever it's doing, as a
# power cut or the OOM killer would. A clean stop lets a step finish first.
kill_dockwell() {
  priv kill -9 "$pid"
  wait "$pid" 2>/dev/null || true
  connected=$(grep -c 'connected to the control plane' "$log" || true)
}

step "A moving tag: a registry of its own, and two images under one tag"
# The product's first user does this most: a tag that moves, redeployed. Only a
# real engine and a real registry say what a pull really does.
registry_port=""
for p in $(seq 15000 15100); do
  if [[ -z "$(ss -ltnH "sport = :$p")" ]]; then
    registry_port=$p
    break
  fi
done
[[ -n "$registry_port" ]] || fail "no port is free for a registry between 15000 and 15100"
docker run -d --name e2e-registry -p "127.0.0.1:$registry_port:5000" "$registry_image" >/dev/null || fail "can't run a registry"
registry_up() {
  for _ in $(seq 100); do
    curl -fsS "http://127.0.0.1:$registry_port/v2/" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  fail "the registry on port $registry_port didn't answer within 10 s"
}
registry_up
moving_repo=127.0.0.1:$registry_port/e2e-app
moving=$moving_repo:latest
moving_dir=$(mktemp -d)
# build_and_push builds an image that serves its word, pushes it under the one tag,
# and says its digest.
build_and_push() {
  printf '%s\n' "$1" >"$moving_dir/index.html"
  printf 'FROM nginx:1.27-alpine\nCOPY index.html /usr/share/nginx/html/index.html\n' >"$moving_dir/Dockerfile"
  docker build -q -t "e2e-moving:$1" "$moving_dir" >/dev/null || fail "can't build the image $1"
  docker tag "e2e-moving:$1" "$moving" || fail "can't tag the image $1"
  docker push "$moving" | awk '/digest: /{ print $3 }'
}
moving_serves() {
  local got=""
  for _ in $(seq 100); do
    got=$(curl -fsS --max-time 2 "http://127.0.0.1:$web_port/" 2>/dev/null) || got=""
    [[ "$got" == "$1" ]] && return 0
    sleep 0.1
  done
  fail "the app serves '$got' on port $web_port, want '$1'"
}
moving_id() {
  local ids
  ids=$(docker ps -a --no-trunc --filter label=com.docker.compose.service=moving --format '{{.ID}}') || fail "docker ps failed"
  [[ "$(wc -l <<<"$ids")" == 1 && -n "$ids" ]] || fail "Docker has these containers for the app, want one: $ids"
  echo "$ids"
}
moving_started() { docker inspect --format '{{.State.StartedAt}}' "$(moving_id)"; }
moving_revisions() { dw services revisions moving --json; }
image_id_of() { docker image inspect --format '{{.Id}}' "$1"; }
digest_first=$(build_and_push first)
[[ "$digest_first" == sha256:* ]] || fail "pushing the first image said its digest is '$digest_first'"
dw services create moving --app-image "$moving" --app-ports "$ports" --yes || fail "services create moving failed"
moving_serves first

step "A moving tag: deploying it again pulls what it points at now, and replaces what runs"
digest_second=$(build_and_push second)
[[ "$digest_second" == sha256:* && "$digest_second" != "$digest_first" ]] ||
  fail "pushing the second image said its digest is '$digest_second', and the first's is '$digest_first'"
# As on any server, only the registry has the new image: the tag here still says
# the first, so nothing but a real pull can move it.
docker tag e2e-moving:first "$moving" || fail "can't put the tag back on the first image"
docker rmi e2e-moving:second "$moving_repo@$digest_second" >/dev/null || fail "can't remove the second image here"
if docker image inspect "$moving_repo@$digest_second" >/dev/null 2>&1; then fail "the second image is still here"; fi
before=$(moving_id)
planned=$(dw services deploy moving --image "$moving" --plan-only --json 2>/dev/null) || fail "planning the deploy failed"
jq -e --arg want "Pulls $moving again, and replaces moving only if that's a newer image" \
  '.change.plan.notes | any(.reason == "pulls_again" and .message == $want)' <<<"$planned" >/dev/null ||
  fail "the plan doesn't say it pulls, and replaces only if that's newer: $planned"
jq -e '.change.plan.servers[0].steps[0].pull == true' <<<"$planned" >/dev/null || fail "the plan's step doesn't pull: $planned"
change=$(jq -r .change.id <<<"$planned")
read_plan=$(dw changes get "$change") || fail "changes get $change failed"
grep -qF "Note: Pulls $moving again, and replaces moving only if that's a newer image." <<<"$read_plan" ||
  fail "the plan a person reads doesn't say it pulls: $read_plan"
grep -qF "Replace if newer" <<<"$read_plan" || fail "the plan a person reads says it replaces for certain: $read_plan"
# Planning changes nothing on the server.
[[ "$(moving_id)" == "$before" ]] || fail "planning replaced the container"
if docker image inspect "$moving_repo@$digest_second" >/dev/null 2>&1; then fail "planning pulled the image"; fi
dw changes apply "$change" --yes || fail "applying the deploy failed"
moving_did() {
  local did
  did=$(dw changes get "$1" --json | jq -r '.change.steps[0].did') || fail "changes get failed"
  [[ "$did" == "$2" ]] || fail "the step says it did $did, want $2: $(dw changes get "$1" --json)"
}
moving_did "$change" PLAN_ACTION_REPLACE
after=$(moving_id)
[[ "$after" != "$before" ]] || fail "with a newer image, the container wasn't replaced"
if docker inspect "$before" >/dev/null 2>&1; then fail "the old container is still there"; fi
runs=$(docker inspect --format '{{.Image}}' "$after") || fail "docker inspect failed"
[[ "$runs" == "$(image_id_of "$moving_repo@$digest_second")" ]] || fail "the container runs $runs, want the second image"
moving_serves second
revisions=$(moving_revisions) || fail "services revisions failed"
jq -e --arg a "$digest_first" --arg b "$digest_second" --arg image "$moving" \
  '(.revisions | length == 2) and .revisions[0].image == $image and .revisions[0].deployments[0].digest == $b and .revisions[1].deployments[0].digest == $a' \
  <<<"$revisions" >/dev/null || fail "the revisions don't say what ran each time: $revisions"
jq -e '(.revisions[1].matchesLatest // false) == false' <<<"$revisions" >/dev/null || fail "r1 ran another image, and reads as matching the latest: $revisions"

step "A moving tag: with nothing newer, nothing happens, and it says so"
before=$(moving_id)
started=$(moving_started)
planned=$(dw services deploy moving --image "$moving" --plan-only --json 2>/dev/null) || fail "planning the deploy failed"
change=$(jq -r .change.id <<<"$planned")
said=$(dw changes apply "$change" --yes 2>&1) || fail "deploying with nothing newer failed: $said"
moving_did "$change" PLAN_ACTION_NONE
grep -qF "$moving is already the newest image, so moving stays as it is" <<<"$said" || fail "with nothing newer, it says: $said"
if grep -qF "Undo it" <<<"$said"; then fail "it offers to undo what did nothing: $said"; fi
[[ "$(moving_id)" == "$before" ]] || fail "with nothing newer, the container was replaced"
[[ "$(moving_started)" == "$started" ]] || fail "with nothing newer, the container restarted"
revisions=$(moving_revisions) || fail "services revisions failed"
jq -e '.revisions | length == 2' <<<"$revisions" >/dev/null || fail "with nothing newer, a revision was written: $revisions"
moving_serves second

step "A moving tag: a rollback runs what that revision ran, by digest, through the registry"
# Off this server, so only the engine's /distribution can say the registry has it,
# and only a pull by digest can bring it back.
docker rmi e2e-moving:first "$moving_repo@$digest_first" >/dev/null || fail "can't remove the first image here"
if docker image inspect "$moving_repo@$digest_first" >/dev/null 2>&1; then fail "the first image is still here"; fi
planned=$(dw services rollback moving --revision 1 --plan-only --json 2>/dev/null) || fail "planning the rollback failed"
jq -e --arg short "sha256:${digest_first:7:12}" '.change.plan.notes | any(.reason == "digest_pinned" and (.message | contains($short)))' <<<"$planned" >/dev/null ||
  fail "the rollback's plan doesn't say it pins what r1 ran: $planned"
if jq -e '.change.plan.notes | any(.reason == "digest_gone")' <<<"$planned" >/dev/null; then fail "the rollback's plan says the image is gone: $planned"; fi
dw changes apply "$(jq -r .change.id <<<"$planned")" --yes || fail "applying the rollback failed"
runs=$(docker inspect --format '{{.Config.Image}}' "$(moving_id)") || fail "docker inspect failed"
[[ "$runs" == "$moving@$digest_first" ]] || fail "after the rollback Docker has the image as $runs, want $moving@$digest_first"
runs=$(docker inspect --format '{{.Image}}' "$(moving_id)") || fail "docker inspect failed"
[[ "$runs" == "$(image_id_of "$moving_repo@$digest_first")" ]] || fail "after the rollback the container runs $runs, want the first image"
moving_serves first
revisions=$(moving_revisions) || fail "services revisions failed"
jq -e --arg a "$digest_first" '.revisions[0].deployments[0].digest == $a and (.revisions[2].matchesLatest // false)' <<<"$revisions" >/dev/null ||
  fail "after the rollback, the revisions say: $revisions"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after the rollback: $drift"
if refused=$(dw services deploy moving --image "$moving@$digest_first" --plan-only 2>&1); then
  fail "deploying the digest it runs planned: $refused"
fi
grep -qF "already runs $moving@$digest_first" <<<"$refused" || fail "deploying the digest it runs said: $refused"
dw compose export >"$moving_dir/export.yaml" || fail "compose export failed"
grep -qF "image: $moving@$digest_first" "$moving_dir/export.yaml" || fail "the export has $(cat "$moving_dir/export.yaml")"
docker compose -f "$moving_dir/export.yaml" config --quiet || fail "docker compose doesn't take the export of a pinned image"

# moving_settled waits for a failed change's rollback to end. That starts just after
# the failure can be read, so it has to stay quiet for a moment to count.
moving_settled() {
  local quiet=0
  for _ in $(seq 240); do
    if [[ "$(dw changes list --json | jq '[(.changes // [])[] | select(.state == "CHANGE_STATE_APPLYING")] | length')" == 0 ]]; then
      quiet=$((quiet + 1))
      [[ "$quiet" -ge 4 ]] && return 0
    else
      quiet=0
    fi
    sleep 0.5
  done
  fail "a change is still applying after 120 s: $(dw changes list --json)"
}
# cant_pull deploys the tag with the registry stopped: it fails saying the fact,
# what follows and the fix, and what runs goes on as it was.
cant_pull() {
  local id started said
  id=$(moving_id)
  started=$(moving_started)
  if said=$(dw services deploy moving --image "$moving" --yes 2>&1); then fail "deploying with the registry stopped applied: $said"; fi
  grep -qF "can't pull $moving again" <<<"$said" || fail "with the registry stopped, the fact isn't there: $said"
  grep -qF "so nothing was replaced, and what runs goes on running" <<<"$said" || fail "with the registry stopped, what follows isn't there: $said"
  grep -qF "Check that its registry answers" <<<"$said" || fail "with the registry stopped, the fix isn't there: $said"
  moving_settled
  [[ "$(moving_id)" == "$id" ]] || fail "a pull that failed replaced the container"
  [[ "$(moving_started)" == "$started" ]] || fail "a pull that failed restarted the container"
  moving_serves "$1"
}

step "A moving tag: with its registry gone, a pinned app fails before anything stops, and goes on serving"
docker stop e2e-registry >/dev/null || fail "can't stop the registry"
cant_pull first

step "A moving tag: so does one on the tag, and that writes no revision"
docker start e2e-registry >/dev/null || fail "can't start the registry again"
registry_up
dw services deploy moving --image "$moving" --yes || fail "deploying the tag again failed"
moving_serves second
written=$(moving_revisions | jq '.revisions | length') || fail "services revisions failed"
docker stop e2e-registry >/dev/null || fail "can't stop the registry"
cant_pull second
[[ "$(moving_revisions | jq '.revisions | length')" == "$written" ]] || fail "a pull that failed wrote a revision: $(moving_revisions)"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after a pull that failed: $drift"

step "A moving tag: with its registry still gone, what isn't running comes back, from what the server has"
written=$(moving_revisions | jq '.revisions | length') || fail "services revisions failed"
moving_comes_back() {
  local said
  said=$(dw services deploy moving --image "$moving" --yes 2>&1) || fail "bringing back what $1 failed: $said"
  grep -qF "Can't pull $moving again" <<<"$said" || fail "it doesn't say it couldn't pull: $said"
  grep -qF "so moving runs the image this server has" <<<"$said" || fail "it doesn't say what it ran: $said"
  if grep -qF "stays as it is" <<<"$said"; then fail "it says it stays as it is, and it wasn't running: $said"; fi
  moving_running
  moving_serves second
}
moving_running() {
  [[ "$(docker inspect --format '{{.State.Running}}' "$(moving_id)")" == true ]] || fail "the app's container isn't running: $(docker ps -a)"
}
docker stop --time 1 "$(moving_id)" >/dev/null || fail "can't stop the app's container"
planned=$(dw services deploy moving --image "$moving" --plan-only --json 2>/dev/null) || fail "planning the deploy of a stopped container failed"
jq -e '.change.plan.servers[0].steps[0].changes | any(.path == "state" and .from == "exited" and .to == "running")' <<<"$planned" >/dev/null ||
  fail "the plan doesn't say it starts what's stopped: $planned"
dw changes discard "$(jq -r .change.id <<<"$planned")" >/dev/null || fail "changes discard failed"
moving_comes_back "was stopped"
docker rm --force "$(moving_id)" >/dev/null || fail "can't remove the app's container"
moving_comes_back "was removed"
[[ "$(moving_revisions | jq '.revisions | length')" == "$written" ]] || fail "bringing back the same image wrote a revision: $(moving_revisions)"

step "A moving tag: with its registry back, a deploy and a rollback each replace what's stopped, and it runs"
docker start e2e-registry >/dev/null || fail "can't start the registry again"
registry_up
docker stop --time 1 "$(moving_id)" >/dev/null || fail "can't stop the app's container"
before=$(moving_id)
said=$(dw services deploy moving --image "$moving" --yes 2>&1) || fail "deploying what's stopped failed: $said"
if grep -qF "stays as it is" <<<"$said"; then fail "it says it stays as it is, and it wasn't running: $said"; fi
[[ "$(moving_id)" != "$before" ]] || fail "what's stopped wasn't replaced"
moving_running
moving_serves second
docker stop --time 1 "$(moving_id)" >/dev/null || fail "can't stop the app's container"
same=$(moving_revisions | jq -r '[.revisions[1:][] | select(.matchesLatest)][0].number') || fail "services revisions failed"
[[ "$same" != null ]] || fail "no revision matches the latest: $(moving_revisions)"
said=$(dw services rollback moving --revision "$same" --yes 2>&1) || fail "rolling back what's stopped was refused: $said"
moving_running
moving_serves second
[[ "$(moving_revisions | jq '.revisions | length')" == "$written" ]] || fail "starting it again wrote a revision: $(moving_revisions)"

step "A moving tag: killed in the middle of a pull, the deploy finishes when Dockwell starts again"
digest_third=$(build_and_push third)
[[ "$digest_third" == sha256:* && "$digest_third" != "$digest_second" ]] || fail "pushing the third image said its digest is '$digest_third'"
docker tag "$moving_repo@$digest_second" "$moving" || fail "can't put the tag back on the second image"
docker rmi e2e-moving:third "$moving_repo@$digest_third" >/dev/null || fail "can't remove the third image here"
before=$(moving_id)
planned=$(dw services deploy moving --image "$moving" --plan-only --json 2>/dev/null) || fail "planning the deploy failed"
change=$(jq -r .change.id <<<"$planned")
# A paused registry holds the pull where it is, for Docker's 15 s or so.
docker pause e2e-registry >/dev/null || fail "can't pause the registry"
dw changes apply "$change" --yes >/dev/null 2>&1 &
applying=$!
for _ in $(seq 100); do
  [[ "$(dw changes get "$change" --json 2>/dev/null | jq -r '.change.steps[0].state // empty')" == STEP_STATE_RUNNING ]] && break
  sleep 0.1
done
[[ "$(dw changes get "$change" --json | jq -r '.change.steps[0].state // empty')" == STEP_STATE_RUNNING ]] ||
  fail "the deploy never reached its pull: $(dw changes get "$change" --json)"
kill_dockwell
wait "$applying" 2>/dev/null || true
docker unpause e2e-registry >/dev/null || fail "can't unpause the registry"
[[ "$(moving_id)" == "$before" ]] || fail "with Dockwell killed in the pull, the container was replaced"
moving_serves second
start_dockwell
wait_for_dockwell
for _ in $(seq 240); do
  state=$(dw changes get "$change" --json | jq -r .change.state) || fail "changes get failed"
  [[ "$state" != CHANGE_STATE_APPLYING ]] && break
  sleep 0.5
done
[[ "$state" == CHANGE_STATE_APPLIED ]] || fail "after starting again, the deploy is $state: $(dw changes get "$change" --json)"
[[ "$(moving_id)" != "$before" ]] || fail "the deploy reads applied, and the old container still runs"
moving_running
moving_serves third
runs=$(docker inspect --format '{{.Image}}' "$(moving_id)") || fail "docker inspect failed"
[[ "$runs" == "$(image_id_of "$moving_repo@$digest_third")" ]] || fail "the container runs $runs, want the third image"
revisions=$(moving_revisions) || fail "services revisions failed"
jq -e --arg written "$written" --arg third "$digest_third" --arg second "$digest_second" \
  '(.revisions | length) == (($written | tonumber) + 1) and .revisions[0].deployments[0].digest == $third and .revisions[0].deployments[0].state == "DEPLOYMENT_STATE_LIVE" and .revisions[1].deployments[0].digest == $second' \
  <<<"$revisions" >/dev/null || fail "the revisions don't say what happened, once: $revisions"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after starting again: $drift"

step "A moving tag: leaving a pin with nothing newer replaces the container, and says it will"
dw services deploy moving --image "$moving@$digest_third" --yes >/dev/null || fail "pinning the third image failed"
[[ "$(docker inspect --format '{{.Config.Image}}' "$(moving_id)")" == "$moving@$digest_third" ]] || fail "the app isn't pinned: $(docker ps -a)"
before=$(moving_id)
planned=$(dw services deploy moving --image "$moving" --plan-only --json 2>/dev/null) || fail "planning the deploy failed"
jq -e '.change.plan.notes | any(.reason == "leaves_pin") and all(.reason != "pulls_again")' <<<"$planned" >/dev/null || fail "the plan claims what it can't: $planned"
change=$(jq -r .change.id <<<"$planned")
read_plan=$(dw changes get "$change") || fail "changes get failed"
grep -qF "in place of the digest it's pinned to" <<<"$read_plan" || fail "the plan doesn't say it leaves the pin: $read_plan"
if grep -qE "if newer|only if" <<<"$read_plan"; then fail "the plan says it may not replace: $read_plan"; fi
said=$(dw changes apply "$change" --yes 2>&1) || fail "leaving the pin failed: $said"
if grep -qE "stays as it is|already the newest" <<<"$said"; then fail "it was replaced, and it says: $said"; fi
moving_did "$change" PLAN_ACTION_REPLACE
[[ "$(moving_id)" != "$before" ]] || fail "the container still names the pin"
moving_running
moving_serves third

dw services delete moving --yes || fail "services delete moving failed"
[[ -z "$(docker ps -aq --filter label=com.docker.compose.service=moving)" ]] || fail "deleting the app left its container"
moving_tidy

step "Ingress: Pebble, Let's Encrypt's test CA, on this machine"
in_ingress=1
tools=$("$(dirname "$0")/ingress-tools.sh" "$tools") || fail "can't get Caddy and Pebble"
domain=app.e2e-dockwell.example.net
cat >"$pebble_dir/pebble.json" <<PEBBLE
{"pebble":{"listenAddress":"127.0.0.1:14000","managementListenAddress":"127.0.0.1:15000",
"certificate":"$pebble_dir/cert.pem","privateKey":"$pebble_dir/key.pem","httpPort":80,"tlsPort":443,
"ocspResponderURL":"","externalAccountBindingRequired":false}}
PEBBLE
"$tools/pebble-challtestsrv" -defaultIPv4 127.0.0.1 -defaultIPv6 "" -dnsserver 127.0.0.1:8053 -doh "" -http01 "" -https01 "" -tlsalpn01 "" \
  -management 127.0.0.1:8055 >"$pebble_dir/challtest.out" 2>&1 &
pebble_pids+=("$!")
PEBBLE_VA_NOSLEEP=1 PEBBLE_WFE_NONCEREJECT=0 "$tools/pebble" -config "$pebble_dir/pebble.json" -dnsserver 127.0.0.1:8053 >"$pebble_dir/pebble.out" 2>&1 &
pebble_pids+=("$!")
for _ in $(seq 100); do
  curl -fsS --cacert "$pebble_dir/cert.pem" https://localhost:14000/dir >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS --cacert "$pebble_dir/cert.pem" https://localhost:14000/dir >/dev/null || fail "Pebble didn't answer within 10 s"

listening() { [[ -n "$(ss -H -ltn "sport = :$1")" ]]; }
if listening 80 || listening 443; then
  fail "something on this machine already holds port 80 or 443, so the ingress's part can't run. Stop it, and run this again"
fi

dw services create web --app-image nginx:1.27-alpine --app-env GREETING=hello --yes || fail "services create web failed"
web=$(docker ps --filter label=com.docker.compose.service=web --format '{{.Names}}') || fail "docker ps failed"
web_id=$(docker inspect --format '{{.Id}}' "$web") || fail "docker inspect $web failed"

step "Ingress: taken ports are refused at plan time, by name"
docker run -d --name e2e-holds-80 -p 80:80 nginx:1.27-alpine >/dev/null || fail "can't run a container on port 80"
set +e
refusal=$(dw routes create www --domain "$domain" --service web --port 80 --yes 2>&1)
status=$?
set -e
[[ "$status" != 0 ]] || fail "a route went through with port 80 taken"
grep -q 'the container e2e-holds-80' <<<"$refusal" || fail "the refusal doesn't name what holds port 80: $refusal"
grep -q 'point that proxy at it' <<<"$refusal" || fail "the refusal doesn't say how to fix it: $refusal"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress started with port 80 taken"; fi
docker rm --force e2e-holds-80 >/dev/null || fail "can't remove the container on port 80"

step "Ingress: a server's first route starts Caddy, with nothing else stopping"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress ran before any route"; fi
# Stopped by hand, and Docker starts it again when it next restarts, as 1Panel's
# OpenResty is, so the plan says so.
docker run -d --name e2e-returns --restart=always -p 80:80 nginx:1.27-alpine >/dev/null || fail "can't run a container on port 80"
docker stop --time 1 e2e-returns >/dev/null || fail "can't stop e2e-returns"
first=$(dw routes create www --domain "$domain" --service web --port 80 --yes) || fail "routes create www failed: $first"
grep -q 'Docker starts e2e-returns again' <<<"$first" || fail "the plan doesn't warn of a stopped container that comes back: $first"
grep -q 'docker update --restart=no e2e-returns' <<<"$first" || fail "the plan doesn't say how to keep it stopped: $first"
docker rm --force e2e-returns >/dev/null || fail "can't remove e2e-returns"
[[ "$(docker inspect --format '{{.Id}}' "$web")" == "$web_id" ]] || fail "adding a route made the service's container again, which is downtime"
hardening=$(docker inspect --format '{{.HostConfig.NetworkMode}} {{.HostConfig.ReadonlyRootfs}} {{.HostConfig.RestartPolicy.Name}}' dockwell-ingress) ||
  fail "there's no dockwell-ingress after the first route"
[[ "$hardening" == "host true unless-stopped" ]] || fail "dockwell-ingress runs as: $hardening, want the host network, a read-only root, and unless-stopped"
capabilities=$(docker inspect --format '{{.HostConfig.CapDrop}} {{.HostConfig.CapAdd}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
[[ "$capabilities" == *ALL*NET_BIND_SERVICE* ]] || fail "dockwell-ingress has the capabilities $capabilities, want all dropped but NET_BIND_SERVICE"
image=$(docker inspect --format '{{.Config.Image}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
[[ "$image" == caddy@sha256:* ]] || fail "dockwell-ingress runs $image, want it by digest"
user=$(docker inspect --format '{{.Config.User}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
if [[ -n "${E2E_ROOT:-}" ]]; then
  caddy_uid=65532
  caddy_user=65532:65532
else
  caddy_uid=$(id -u)
  caddy_user=$(id -u):$(id -g)
fi
[[ "$user" == "$caddy_user" ]] || fail "dockwell-ingress runs as $user, want $caddy_user"
says true docker inspect --format '{{.State.Running}}' dockwell-ingress || fail "dockwell-ingress isn't running"
running_as=$(docker exec dockwell-ingress id -u) || fail "can't ask dockwell-ingress who it runs as"
[[ "$running_as" == "$caddy_uid" ]] || fail "Caddy's process runs as uid $running_as, want $caddy_uid"
limits=$(docker inspect --format '{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.UsernsMode}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
[[ "$limits" == "1073741824 1073741824 host" ]] || fail "dockwell-ingress has memory, swap and user namespace $limits, want 1 GiB, no swap and the server's"
says '^CADDY_ADMIN=unix//run/dockwell/admin\.sock\|0600$' docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' dockwell-ingress ||
  fail "dockwell-ingress has no CADDY_ADMIN naming its socket, so an empty last config would open the admin API on TCP"
for dir in data config run; do
  owned=$(priv stat -c '%u %a' "$data/ingress/$dir") || fail "can't read $data/ingress/$dir"
  [[ "$owned" == "$caddy_uid 700" ]] || fail "ingress/$dir is owned and has mode $owned, want $caddy_uid 700"
done
if [[ -n "${E2E_ROOT:-}" ]]; then
  # What Caddy reads as it starts is root's, so nothing it does can change where a
  # write of root's lands, or what it reads next time.
  for file in boot boot/bootstrap.json boot/acme-roots.pem; do
    owned=$(priv stat -c '%u' "$data/ingress/$file") || fail "can't read $data/ingress/$file"
    [[ "$owned" == 0 ]] || fail "ingress/$file is owned by uid $owned, want root"
  done
  [[ "$(priv stat -c '%u %a' "$data/ingress/last-good.json")" == "0 600" ]] || fail "the agent's copy of Caddy's config isn't root's alone"
fi
if docker exec dockwell-ingress touch /etc/dockwell/x 2>/dev/null; then fail "Caddy can write to what it reads as it starts"; fi
for _ in $(seq 100); do
  says true docker inspect --format '{{.State.Running}}' dockwell-ingress-keep 2>/dev/null && break
  sleep 0.1
done
says true docker inspect --format '{{.State.Running}}' dockwell-ingress-keep 2>/dev/null || fail "nothing keeps the ingress network from being pruned: $(docker logs dockwell-ingress-keep 2>&1)"
for p in 80 443; do
  listening "$p" || fail "nothing listens on port $p"
done
[[ -n "$(ss -H -lun 'sport = :443')" ]] || fail "nothing listens on UDP 443, for HTTP/3"
curl -fsS --max-time 2 http://127.0.0.1:2019/config/ >/dev/null 2>&1 && fail "Caddy's admin API answers on TCP"
priv test -S "$data/ingress/run/admin.sock" || fail "there's no admin socket at $data/ingress/run/admin.sock"

step "Ingress: the network is kept to itself"
network=$(docker network inspect --format '{{.Internal}} {{index .Options "com.docker.network.bridge.enable_icc"}}' dockwell_ingress) ||
  fail "there's no network dockwell_ingress"
[[ "$network" == "true false" ]] || fail "dockwell_ingress is internal and enable_icc: $network, want true and false"
address=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAddress}}' "$web") || fail "web isn't on dockwell_ingress"
static=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAMConfig.IPv4Address}}' "$web") || fail "web has no static address on dockwell_ingress"
[[ "$address" == "$static" ]] || fail "web is at $address, and asked for $static"
says nginx curl -fsS --max-time 5 "http://$address/" || fail "this machine can't reach web on the ingress network, at $address, as Caddy must"
# wget_from_network says how wget exited inside a container on the network, so a
# docker run that fails for any other reason doesn't read as the network holding.
wget_from_network() {
  docker run --rm --network dockwell_ingress nginx:1.27-alpine sh -c "wget -q -T 3 -O- $1 >/dev/null 2>&1; echo wget exited \$?"
}
reached=$(wget_from_network "http://$address/") || fail "can't run a container on the ingress network: $reached"
[[ "$reached" == 'wget exited '[1-9]* ]] || fail "a container on the ingress network reached web, as $reached: enable_icc: false isn't keeping them apart"
reached=$(wget_from_network http://1.1.1.1/) || fail "can't run a container on the ingress network: $reached"
[[ "$reached" == 'wget exited '[1-9]* ]] || fail "a container on the ingress network got out through it, as $reached: internal: true isn't holding"
outbound=$(docker exec "$web" wget -q -T 5 --spider http://localhost/ 2>&1 && echo up) || fail "web stopped answering itself: $outbound"
default_route=$(docker exec "$web" ip route show default 2>/dev/null) || default_route=""
ingress_gateway=$(docker network inspect --format '{{(index .IPAM.Config 0).Gateway}}' dockwell_ingress) || ingress_gateway=""
if [[ -n "$ingress_gateway" ]] && grep -qF "$ingress_gateway" <<<"$default_route"; then
  fail "web's default route moved to the ingress network: $default_route"
fi

step "Ingress: a real certificate, from a real CA, and a request over HTTPS to a real container"
state=""
for _ in $(seq 120); do
  listed=$(dw domains list --json 2>/dev/null) || listed='{}'
  state=$(jq -r --arg d "$domain" '.domains[]? | select(.name == $d) | .servers[0].status.certificate.state // empty' <<<"$listed")
  [[ "$state" == CERTIFICATE_STATE_ISSUED ]] && break
  sleep 0.5
done
[[ "$state" == CERTIFICATE_STATE_ISSUED ]] || fail "$domain's certificate reads '$state' after 60 s, want issued"
curl -fsS --cacert "$pebble_dir/cert.pem" https://localhost:15000/roots/0 >"$pebble_dir/root.pem" || fail "can't read Pebble's root"
for intermediate in 0 1; do
  curl -fsS --cacert "$pebble_dir/cert.pem" "https://localhost:15000/intermediates/$intermediate" >>"$pebble_dir/root.pem" 2>/dev/null || true
done
over_https() { curl -fsS --max-time 10 --cacert "$pebble_dir/root.pem" --resolve "$domain:443:127.0.0.1" "$@" "https://$domain/"; }
says nginx over_https || fail "https://$domain/ doesn't serve web through Caddy: $(over_https -v 2>&1)"
redirect=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code} %{redirect_url}' --resolve "$domain:80:127.0.0.1" "http://$domain/x?y=1") || fail "http://$domain/ didn't answer"
[[ "$redirect" == "308 https://$domain/x?y=1" ]] || fail "http://$domain/x?y=1 answers $redirect, want a redirect to HTTPS"

step "Ingress: the service reads the real client, and Caddy owns the header"
over_https -H 'X-Forwarded-For: 6.6.6.6' >/dev/null || fail "the request with a forged header failed"
sleep 0.5
last_log() { docker logs --tail 5 "$web" 2>&1 | tail -1; }
says '"127\.0\.0\.1"$' last_log ||
  fail "web's last request doesn't read as from 127.0.0.1, in X-Forwarded-For: $(docker logs --tail 5 "$web" 2>&1)"
logs=$(docker logs --tail 5 "$web" 2>&1) || fail "docker logs $web failed"
if grep -q '6\.6\.6\.6' <<<"$logs"; then fail "a forged X-Forwarded-For reached web"; fi
# Whether this machine has IPv6 on loopback is the kernel's to say, as a curl that
# fails for any other reason must fail this.
if grep -q '^00000000000000000000000000000001' /proc/net/if_inet6 2>/dev/null; then
  curl -fsS --max-time 5 --cacert "$pebble_dir/root.pem" --resolve "$domain:443:[::1]" -o /dev/null "https://$domain/" ||
    fail "https://$domain/ doesn't answer over IPv6"
  sleep 0.5
  says '"::1"$' last_log ||
    fail "over IPv6, web's last request doesn't read as from ::1, so a client's real address is lost: $(docker logs --tail 5 "$web" 2>&1)"
else
  echo "This machine has no IPv6 on loopback, so the client's real address over IPv6 wasn't checked"
fi

step "Ingress: a replaced service keeps its address, and goes on serving"
dw services update web --app-image nginx:1.27-alpine --app-env GREETING=hi --yes || fail "services update web failed"
[[ "$(docker inspect --format '{{.Id}}' "$web")" != "$web_id" ]] || fail "the update didn't replace web's container"
now=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAMConfig.IPv4Address}}' "$web") || fail "the new container isn't on dockwell_ingress"
[[ "$now" == "$static" ]] || fail "the new container is at $now, want the instance's address, $static"
says nginx over_https || fail "after the update, https://$domain/ doesn't serve web"
listed=$(docker compose -p "$project" ps --format '{{.Service}}') || fail "docker compose -p $project ps failed"
grep -qx web <<<"$listed" || fail "docker compose doesn't list web on the ingress network: $listed"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after a route and a replace: $drift"

step "Ingress: traffic keeps flowing with Dockwell down, and through a restart of Caddy"
stop_dockwell
says nginx over_https || fail "with Dockwell down, https://$domain/ stopped serving"
docker restart --time 5 dockwell-ingress >/dev/null || fail "docker restart dockwell-ingress failed"
for _ in $(seq 100); do
  over_https >/dev/null 2>&1 && break
  sleep 0.1
done
says nginx over_https || fail "with Dockwell down, Caddy came back without its last config"
# Docker itself restarting brings back everything, and gives each its address, with
# no agent there. It needs root, and a Docker that systemd runs.
if [[ -n "${E2E_ROOT:-}" ]] && systemctl is-active --quiet docker 2>/dev/null; then
  step "Ingress: traffic comes back with Docker itself restarted, and Dockwell down"
  sudo -n systemctl restart docker || fail "can't restart Docker"
  for _ in $(seq 300); do
    over_https >/dev/null 2>&1 && break
    sleep 0.1
  done
  says nginx over_https || fail "with Docker restarted and Dockwell down, https://$domain/ didn't come back"
  now=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAMConfig.IPv4Address}}' "$web") || fail "web isn't on dockwell_ingress after Docker restarted"
  [[ "$now" == "$static" ]] || fail "after Docker restarted, web is at $now, want $static"
else
  echo "Docker wasn't restarted, as that needs E2E_ROOT and systemd. CI has both"
fi
start_dockwell
wait_for_dockwell

autosave=$data/ingress/config/caddy/autosave.json
serves() {
  for _ in $(seq 300); do
    over_https >/dev/null 2>&1 && break
    sleep 0.1
  done
  says nginx over_https || fail "$1"
}
admin() { priv curl -fsS --max-time 2 --unix-socket "$data/ingress/run/admin.sock" "http://caddy$1"; }

step "Ingress: a last config left empty comes back, with the admin API on its socket alone"
stop_dockwell
docker stop --time 5 dockwell-ingress >/dev/null || fail "docker stop dockwell-ingress failed"
priv truncate -s 0 "$autosave" || fail "can't empty $autosave"
docker start dockwell-ingress >/dev/null || fail "docker start dockwell-ingress failed"
for _ in $(seq 100); do
  admin /config/ >/dev/null 2>&1 && break
  sleep 0.1
done
[[ "$(admin /config/)" == null ]] || fail "from an empty last config, Caddy runs: $(admin /config/)"
if curl -fsS --max-time 2 http://127.0.0.1:2019/config/ >/dev/null 2>&1; then fail "from an empty last config, Caddy's admin API answers on TCP"; fi
start_dockwell
wait_for_dockwell
serves "the agent didn't put back Caddy's last config, left empty"
[[ "$(priv stat -c '%u' "$autosave")" == "$caddy_uid" ]] || fail "the last config is owned by uid $(priv stat -c '%u' "$autosave"), want $caddy_uid"

step "Ingress: a last config cut short comes back too, rather than restarting Caddy for ever"
stop_dockwell
docker stop --time 5 dockwell-ingress >/dev/null || fail "docker stop dockwell-ingress failed"
priv truncate -s 100 "$autosave" || fail "can't cut $autosave short"
docker start dockwell-ingress >/dev/null 2>&1 || true
sleep 2
if over_https >/dev/null 2>&1; then fail "Caddy served from half a config, so this proves nothing"; fi
start_dockwell
wait_for_dockwell
serves "the agent didn't put back Caddy's last config, cut short"

step "Ingress: docker network prune can't take the network, and a network lost comes back on its subnet"
subnet=$(docker network inspect --format '{{(index .IPAM.Config 0).Subnet}}' dockwell_ingress) || fail "there's no network dockwell_ingress"
stop_dockwell
docker stop --time 1 "$web" >/dev/null || fail "docker stop $web failed"
docker network prune --force >/dev/null || fail "docker network prune failed"
docker network inspect dockwell_ingress >/dev/null 2>&1 || fail "docker network prune took the ingress network, with the keeper on it"
docker rm --force dockwell-ingress-keep >/dev/null || fail "can't remove the keeper"
docker network prune --force >/dev/null || fail "docker network prune failed"
if docker network inspect dockwell_ingress >/dev/null 2>&1; then fail "docker network prune left the network with no running container on it, so this proves nothing"; fi
start_dockwell
wait_for_dockwell
for _ in $(seq 300); do
  [[ "$(docker inspect --format '{{.State.Running}}' dockwell-ingress-keep 2>/dev/null)" == true ]] && break
  sleep 0.1
done
[[ "$(docker inspect --format '{{.State.Running}}' dockwell-ingress-keep 2>/dev/null)" == true ]] || fail "the keeper didn't come back"
[[ "$(docker network inspect --format '{{(index .IPAM.Config 0).Subnet}}' dockwell_ingress)" == "$subnet" ]] || fail "the network came back off its subnet, $subnet: $(docker network inspect dockwell_ingress)"
# A stopped container names the old network's ID, and can't start until it comes
# off it, which the agent has seen to.
docker start "$web" >/dev/null || fail "web can't start on the network made again"
now=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAMConfig.IPv4Address}}' "$web") || fail "web isn't on dockwell_ingress"
[[ "$now" == "$static" ]] || fail "on the network made again, web is at $now, want $static"
serves "on the network made again, https://$domain/ doesn't serve web"

step "Ingress: another network on its subnet closes the routes, names it, and reuses nothing"
stop_dockwell
docker stop --time 1 "$web" >/dev/null || fail "docker stop $web failed"
docker rm --force dockwell-ingress-keep >/dev/null || fail "can't remove the keeper"
docker network rm dockwell_ingress >/dev/null || fail "can't remove dockwell_ingress"
docker network create --subnet "$subnet" e2e-took-subnet >/dev/null || fail "can't make a network on $subnet"
start_dockwell
wait_for_dockwell
code=""
for _ in $(seq 300); do
  code=$(curl -sS --max-time 5 --cacert "$pebble_dir/root.pem" --resolve "$domain:443:127.0.0.1" -o /dev/null -w '%{http_code}' "https://$domain/" 2>/dev/null) || code=""
  [[ "$code" == 503 ]] && break
  sleep 0.1
done
[[ "$code" == 503 ]] || fail "with another network on its subnet, https://$domain/ answers $code, want 503"
ingress_problem() { dw servers list --json 2>/dev/null | jq -r '.servers[0].ingress'; }
said=""
for _ in $(seq 300); do
  said=$(ingress_problem)
  jq -e '.problem == "INGRESS_PROBLEM_SUBNET_TAKEN"' <<<"$said" >/dev/null 2>&1 && break
  sleep 0.1
done
jq -e '.problem == "INGRESS_PROBLEM_SUBNET_TAKEN"' <<<"$said" >/dev/null || fail "the API doesn't say the subnet is taken: $said"
jq -e '.message | contains("e2e-took-subnet")' <<<"$said" >/dev/null || fail "the API doesn't name who took it: $said"
if docker network inspect dockwell_ingress >/dev/null 2>&1; then fail "the network was made anyway"; fi
docker network rm e2e-took-subnet >/dev/null || fail "can't remove e2e-took-subnet"
stop_dockwell
start_dockwell
wait_for_dockwell
for _ in $(seq 300); do
  docker network inspect dockwell_ingress >/dev/null 2>&1 && docker start "$web" >/dev/null 2>&1 && break
  sleep 0.1
done
serves "with its subnet free, https://$domain/ doesn't serve web"

step "Ingress: a proxy back on the ports is named, Caddy isn't made again for it, and a route can still go"
dw routes create api --domain "$domain" --path /api --service web --port 80 --yes >/dev/null || fail "routes create api failed"
stop_dockwell
docker stop --time 5 dockwell-ingress >/dev/null || fail "docker stop dockwell-ingress failed"
caddy_id=$(docker inspect --format '{{.Id}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
docker run -d --name e2e-holds-80 --restart=always -p 80:80 nginx:1.27-alpine >/dev/null || fail "can't run a container on port 80"
start_dockwell
wait_for_dockwell
said=""
for _ in $(seq 300); do
  said=$(ingress_problem)
  jq -e '.problem == "INGRESS_PROBLEM_PORTS_TAKEN"' <<<"$said" >/dev/null 2>&1 && break
  sleep 0.1
done
jq -e '.problem == "INGRESS_PROBLEM_PORTS_TAKEN" and (.holders | index("e2e-holds-80") != null)' <<<"$said" >/dev/null || fail "the API doesn't name what holds the ports: $said"
grep -q 'docker update --restart=no e2e-holds-80' "$log" || fail "the log doesn't name what holds the ports, with the fix"
[[ "$(docker inspect --format '{{.Id}}' dockwell-ingress)" == "$caddy_id" ]] || fail "dockwell-ingress was made again with the ports taken"
# Removing a route must always work, and doesn't start Caddy to do it.
dw routes delete api --yes >/dev/null || fail "a route couldn't be removed with another proxy on the ports"
if priv grep -q '/api' "$autosave"; then fail "the route removed with Caddy down is still in its last config"; fi
docker rm --force e2e-holds-80 >/dev/null || fail "can't remove e2e-holds-80"
stop_dockwell
start_dockwell
wait_for_dockwell
serves "with the ports free again, https://$domain/ doesn't serve web"

step "Ingress: a hand edit reads as drift, and a revert puts it back"
hand='{"handler":"static_response","body":"by hand"}'
priv curl -fsS --unix-socket "$data/ingress/run/admin.sock" -X PUT -H 'Content-Type: application/json' -d "$hand" \
  "http://caddy/id/dockwell.$project.www/handle/0" >/dev/null || fail "can't edit the route by hand"
says 'by hand' over_https || fail "the hand edit didn't take"
drifted=""
for _ in $(seq 60); do
  drift=$(dw drift list --json 2>/dev/null) || drift='{}'
  drifted=$(jq -r '.drift[]? | select(.resource.kind == "route") | .resourceId' <<<"$drift")
  [[ -n "$drifted" ]] && break
  sleep 0.5
done
[[ -n "$drifted" ]] || fail "a hand edit to the route doesn't read as drift: $drift"
says 'by hand' over_https || fail "Dockwell reverted a hand edit on its own: drift is reported, never silently reverted"
server=$(jq -r '.drift[] | select(.resource.kind == "route") | .serverId' <<<"$drift")
dw drift revert --server-id "$server" --resource-id "$drifted" --yes || fail "drift revert failed"
says nginx over_https || fail "after reverting, https://$domain/ doesn't serve web"

step "Ingress: the last route stops Caddy and frees the ports, and its certificate stays"
certificate=$(priv find "$data/ingress/data/caddy/certificates" -name "$domain.crt") || fail "can't look for the certificate"
[[ -n "$certificate" ]] || fail "there's no certificate on disk for $domain"
before=$(priv openssl x509 -in "$certificate" -noout -serial) || fail "can't read the certificate"
dw routes delete www --yes || fail "routes delete www failed"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "dockwell-ingress is there after the last route"; fi
if docker network inspect dockwell_ingress >/dev/null 2>&1; then fail "the network dockwell_ingress is there after the last route"; fi
if docker inspect dockwell-ingress-keep >/dev/null 2>&1; then fail "dockwell-ingress-keep is there after the last route"; fi
if priv test -e "$data/ingress/last-good.json"; then fail "the agent's copy of Caddy's config is there after the last route"; fi
for p in 80 443; do
  if listening "$p"; then fail "port $p is still held after the last route"; fi
done
priv test -f "$certificate" || fail "the certificate went with Caddy"
priv test -d "$data/ingress/data/caddy/acme" || fail "the ACME account went with Caddy"
on=$(docker inspect --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$web") || fail "docker inspect $web failed"
if grep -q dockwell_ingress <<<"$on"; then fail "web stayed on the ingress network"; fi

step "Ingress: a route again reuses its certificate"
dw routes create www --domain "$domain" --service web --port 80 --yes || fail "routes create www, again, failed"
for _ in $(seq 100); do
  over_https >/dev/null 2>&1 && break
  sleep 0.1
done
says nginx over_https || fail "with the route back, https://$domain/ doesn't serve web"
after=$(priv openssl x509 -in "$certificate" -noout -serial) || fail "can't read the certificate"
[[ "$after" == "$before" ]] || fail "a new certificate was issued, $after, where the one kept was $before"
dw routes delete www --yes || fail "routes delete www, again, failed"

# The network prune above took the imported file's network, as nothing was on it by
# then, and it rightly stays as drift. So these routes are held to what they add.
drifted() { dw drift list --json | jq -c '[(.drift // [])[] | .resource] | sort_by(.kind, .name)'; }
drift_before=$(drifted) || fail "drift list failed"
[[ "$(jq -r 'map(.kind + " " + .name) | join(",")' <<<"$drift_before")" == "network back" ]] ||
  fail "before these routes, drift should be only the network the prune took, and is: $drift_before"

step "Ingress: a route that takes no certificate serves plain HTTP, on port 80 alone"
behind=behind.e2e-dockwell.example.net
# Each order Pebble takes, as it logs them. Nothing here may ask it for one.
orders() { grep -c 'Added order' "$pebble_dir/pebble.out" || true; }
orders_before=$(orders)
# An earlier step got a certificate from Pebble, or counting them proves nothing.
[[ "$orders_before" -gt 0 ]] || fail "Pebble has taken no order yet, so a count that doesn't move says nothing"
made=$(dw routes create behind --domain "$behind" --service web --port 80 --tls off --yes 2>&1) || fail "routes create behind failed: $made"
[[ "$made" == *"unencrypted"* ]] || fail "the plan doesn't say traffic reaches this server unencrypted: $made"
[[ "$made" == *"takes ports 80/tcp."* ]] || fail "the plan doesn't say Caddy takes port 80 alone: $made"
[[ "$made" == *"answers only what runs on"* ]] || fail "the plan doesn't say who it answers: $made"
listening 80 || fail "nothing listens on port 80 for a route that takes no certificate"
if listening 443; then fail "something listens on port 443 with no route needing it"; fi
[[ -z "$(ss -H -lun 'sport = :443')" ]] || fail "something listens on UDP 443 with no route needing it"
plain_http() { curl -sS --max-time 10 --resolve "$behind:80:127.0.0.1" "$@" "http://$behind/"; }
answer=$(plain_http -o /dev/null -w '%{http_code} %{redirect_url}') || fail "http://$behind/ didn't answer"
[[ "$answer" == "200 " ]] || fail "http://$behind/ answers $answer, want it served, as a redirect would loop behind a tunnel"
body=$(plain_http) || fail "http://$behind/ didn't answer"
[[ "$body" == *nginx* ]] || fail "http://$behind/ doesn't serve web: $body"
if curl -sS --max-time 3 -o /dev/null --resolve "$behind:443:127.0.0.1" "https://$behind/" 2>/dev/null; then fail "https://$behind/ answered, with nothing asked of it"; fi
state=""
for _ in $(seq 180); do
  listed=$(dw domains list --json 2>/dev/null) || listed='{}'
  state=$(jq -r --arg d "$behind" '.domains[]? | select(.name == $d) | .servers[0].status.certificate.state // empty' <<<"$listed")
  [[ "$state" == CERTIFICATE_STATE_NONE ]] && break
  sleep 0.5
done
[[ "$state" == CERTIFICATE_STATE_NONE ]] || fail "$behind's certificate reads '$state' after 90 s, want none"
message=$(jq -r --arg d "$behind" '.domains[] | select(.name == $d) | .servers[0].status.certificate.message' <<<"$listed")
[[ "$message" == *"TLS ends before this server"* ]] || fail "$behind's certificate says: $message"
url=$(jq -r --arg d "$behind" '.domains[] | select(.name == $d) | .url' <<<"$listed")
[[ "$url" == "http://$behind" ]] || fail "$behind answers at $url, as the API has it"
[[ "$(orders)" == "$orders_before" ]] || fail "a route that takes no certificate asked Pebble for one: $(grep 'Added order' "$pebble_dir/pebble.out")"
held=$(priv find "$data/ingress/data/caddy/certificates" -name "$behind.crt") || fail "can't look for a certificate"
[[ -z "$held" ]] || fail "there's a certificate for a route that takes none: $held"

step "Ingress: a route with no certificate answers what it names, not the internet"
# This machine's own address stands for a stranger, as a request to it comes from it.
stranger=$(hostname -I | awk '{ print $1 }')
[[ -n "$stranger" && "$stranger" != 127.* ]] || fail "this machine has no address but loopback, so this proves nothing: '$stranger'"
straight() { curl -sS --max-time 10 -H "Host: $1" -o /dev/null -w '%{http_code}' "http://$stranger/"; }
nobody=$(curl -sS --max-time 10 -H 'Host: nobody.e2e-dockwell.example.net' "http://$stranger/") || fail "a name with no route didn't answer"
[[ "$nobody" == *"no site at this address"* ]] || fail "a name with no route says: $nobody"
reached=$(curl -sS --max-time 10 -H "Host: $behind" "http://$stranger/") || fail "http://$stranger/ didn't answer"
[[ "$reached" == "$nobody" ]] || fail "a stranger at this server's own address got this from a route behind a tunnel, not what a name with no route gets: $reached"

# Each way docs/install.md says to run something like cloudflared, with a container
# of nginx's image doing what it would. wget says the status when it fails, so a
# timeout doesn't read as a refusal.
from_host=$(docker run --rm --network host nginx:1.27-alpine wget -q -T 5 -O- --header "Host: $behind" http://127.0.0.1:80/) ||
  fail "a container on the server's own network can't reach Caddy at 127.0.0.1:80"
[[ "$from_host" == *nginx* ]] || fail "from the server's own network, 127.0.0.1:80 answers: $from_host"
on_bridge() { docker run --rm --add-host host.docker.internal:host-gateway nginx:1.27-alpine sh -c "wget -T 5 -O- --header 'Host: $1' http://host.docker.internal:80/ 2>&1" || true; }
from_bridge=$(on_bridge "$behind")
[[ "$from_bridge" == *"404"* ]] || fail "a container on a bridge, with its range not named, got: $from_bridge"
bridge=$(docker network inspect --format '{{(index .IPAM.Config 0).Subnet}}' bridge) || fail "docker network inspect bridge failed"
bridged=bridged.e2e-dockwell.example.net
set +e
planned=$(dw routes create bridged --domain "$bridged" --service web --port 80 --tls off --trusted-proxies "$bridge" --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "a route naming a proxy exited $status, want 4: it waits for a person: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
person_apply "$(jq -r '.changes[0].id' <<<"$changes")"
from_bridge=$(on_bridge "$bridged")
[[ "$from_bridge" == *nginx* ]] || fail "a container on a bridge, with its range named, got: $from_bridge"
dw routes delete bridged --yes || fail "routes delete bridged failed"

# Anyone takes a person, and says so.
open=open.e2e-dockwell.example.net
set +e
planned=$(dw routes create open --domain "$open" --service web --port 80 --tls off --plain-for-anyone --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "a route for anyone exited $status, want 4: it waits for a person: $planned"
[[ "$planned" == *"answers anyone who can reach"* ]] || fail "the plan doesn't say it answers anyone: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
anyone=$(jq -r '.changes[0].id' <<<"$changes")
token_refused "$anyone"
person_apply "$anyone"
[[ "$(straight "$open")" == 200 ]] || fail "a route for anyone doesn't answer at $stranger"
dw routes delete open --yes || fail "routes delete open failed"

step "Ingress: a route to a port on the server needs a person, names the port, and believes the proxy it names"
on_server=$((web_port + 1))
docker run -d --name e2e-on-server -p "127.0.0.1:$on_server:80" nginx:1.27-alpine >/dev/null || fail "can't run something on port $on_server"
for _ in $(seq 100); do
  curl -fsS --max-time 1 -o /dev/null "http://127.0.0.1:$on_server/" 2>/dev/null && break
  sleep 0.1
done
curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:$on_server/" || fail "nothing answers on port $on_server, so this proves nothing"
through=through.e2e-dockwell.example.net
set +e
planned=$(dw routes create through --domain "$through" --host-port "$on_server" --tls off --trusted-proxies 127.0.0.1 --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "a route to a port on the server exited $status, want 4: it waits for a person: $planned"
[[ "$planned" == *"127.0.0.1:$on_server"* ]] || fail "the plan doesn't name the port: $planned"
[[ "$planned" == *"127.0.0.1/32 as a proxy"* ]] || fail "the plan doesn't name the proxy: $planned"
[[ "$planned" == *"sees each caller as this server"* ]] || fail "the plan doesn't say what's there sees everyone as local: $planned"
[[ "$planned" == *"the container e2e-on-server"* ]] || fail "the plan doesn't name what listens there: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
reach=$(jq -r '.changes[0].id' <<<"$changes")
[[ "$reach" == chg_* ]] || fail "changes list has no change: $changes"
# The CLI stops short of asking, so this is the server's own refusal.
token_refused "$reach"
person_apply "$reach"
answer=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' --resolve "$through:80:127.0.0.1" -H 'X-Forwarded-For: 6.6.6.6, 198.51.100.7' "http://$through/") ||
  fail "http://$through/ didn't answer"
[[ "$answer" == 200 ]] || fail "http://$through/ answers $answer"
sleep 0.5
seen=$(docker logs --tail 5 e2e-on-server 2>&1) || fail "docker logs e2e-on-server failed"
seen=${seen##*$'\n'}
[[ "$seen" == *'"198.51.100.7"' ]] || fail "what's on the server doesn't read the address its proxy found, in X-Forwarded-For: $seen"
if [[ "$seen" == *6.6.6.6* ]]; then fail "what a client sent itself reached what's on the server: $seen"; fi
# And the same request, on the route that names no proxy, believes none of it.
answer=$(plain_http -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 6.6.6.6') || fail "http://$behind/ didn't answer"
[[ "$answer" == 200 ]] || fail "http://$behind/ answers $answer"
sleep 0.5
seen=$(docker logs --tail 5 "$web" 2>&1) || fail "docker logs $web failed"
seen=${seen##*$'\n'}
[[ "$seen" == *'"127.0.0.1"' ]] || fail "beside a route that trusts a proxy, one that doesn't reads: $seen"
[[ "$(drifted)" == "$drift_before" ]] || fail "drift after routes behind a tunnel: $(dw drift list --json), and before them it was $drift_before"
set +e
own=$(dw routes create own --domain own.e2e-dockwell.example.net --host-port "$port" --tls off --yes 2>&1)
status=$?
set -e
[[ "$status" != 0 ]] || fail "a route went through to the panel's own port"
[[ "$own" == *"one Dockwell listens on itself"* ]] || fail "the refusal of the panel's own port says: $own"
set +e
own=$(dw routes create own --domain own.e2e-dockwell.example.net --host-port 2375 --tls off --yes 2>&1)
status=$?
set -e
[[ "$status" != 0 ]] || fail "a route went through to Docker's API"
[[ "$own" == *"Docker's API"* ]] || fail "the refusal of Docker's API says: $own"

step "Ingress: a route with a certificate takes port 443 beside them, and frees it as it goes"
took=$(dw routes create www --domain "$domain" --service web --port 80 --yes 2>&1) || fail "routes create www failed: $took"
[[ "$took" == *"takes ports 443/tcp and 443/udp too"* ]] || fail "the plan doesn't say Caddy takes port 443: $took"
for p in 80 443; do
  listening "$p" || fail "nothing listens on port $p with routes of both kinds"
done
for _ in $(seq 300); do
  over_https >/dev/null 2>&1 && break
  sleep 0.1
done
body=$(over_https) || fail "https://$domain/ doesn't answer beside a plain route"
[[ "$body" == *nginx* ]] || fail "https://$domain/ doesn't serve web: $body"
redirect=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' --resolve "$domain:80:127.0.0.1" "http://$domain/") || fail "http://$domain/ didn't answer"
[[ "$redirect" == 308 ]] || fail "http://$domain/ answers $redirect, want its redirect"
answer=$(plain_http -o /dev/null -w '%{http_code}') || fail "http://$behind/ didn't answer"
[[ "$answer" == 200 ]] || fail "beside a route with a certificate, http://$behind/ answers $answer"
# Taking it off HTTPS is a person's, and says what stops.
set +e
planned=$(dw routes update www --domain "$domain" --service web --port 80 --tls off --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "taking a live domain off HTTPS exited $status, want 4: it waits for a person: $planned"
[[ "$planned" == *"which stops"* && "$planned" == *HSTS* ]] || fail "the plan doesn't say HTTPS stops: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
taking=$(jq -r '.changes[0].id' <<<"$changes")
token_refused "$taking"
dw changes discard "$taking" >/dev/null || fail "changes discard failed"
freed=$(dw routes delete www --yes 2>&1) || fail "routes delete www failed: $freed"
[[ "$freed" == *"frees ports 443/tcp and 443/udp"* ]] || fail "the plan doesn't say Caddy frees port 443: $freed"
if listening 443; then fail "port 443 is still held with no route needing it"; fi
answer=$(plain_http -o /dev/null -w '%{http_code}') || fail "http://$behind/ didn't answer"
[[ "$answer" == 200 ]] || fail "after port 443 was freed, http://$behind/ answers $answer"

# The script removes what it was installed as, so it runs only where nothing is.
if [[ -n "${E2E_ROOT:-}" && ! -e /usr/local/bin/dockwell ]]; then
  step "Ingress: uninstalling says what stays, and frees the ports when asked"
  install_script=$(dirname "$0")/install.sh
  stop_dockwell
  said=$(sudo -n sh "$install_script" --uninstall 2>&1) || fail "uninstalling failed: $said"
  [[ "$said" == *"--uninstall --remove-ingress"* ]] || fail "uninstalling doesn't say how to free the ports: $said"
  [[ "$said" == *"before you delete"* ]] || fail "uninstalling doesn't say to do that before deleting the data: $said"
  [[ "$(docker inspect --format '{{.State.Running}}' dockwell-ingress)" == true ]] || fail "uninstalling stopped Caddy, which it wasn't asked to"
  answer=$(plain_http -o /dev/null -w '%{http_code}') || fail "http://$behind/ didn't answer"
  [[ "$answer" == 200 ]] || fail "uninstalling stopped a route answering: $answer"
  # A stopped service comes off the network too, or it couldn't start again.
  docker stop --time 1 "$web" >/dev/null || fail "can't stop web"
  said=$(sudo -n sh "$install_script" --uninstall --remove-ingress 2>&1) || fail "removing the ingress failed: $said"
  [[ "$said" == *"the ports it held are free"* ]] || fail "removing the ingress says: $said"
  if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "dockwell-ingress is there after removing the ingress"; fi
  if docker inspect dockwell-ingress-keep >/dev/null 2>&1; then fail "dockwell-ingress-keep is there after removing the ingress"; fi
  if docker network inspect dockwell_ingress >/dev/null 2>&1; then fail "the network dockwell_ingress is there after removing the ingress"; fi
  if listening 80; then fail "port 80 is still held after removing the ingress"; fi
  docker start "$web" >/dev/null || fail "a stopped service can't start after removing the ingress: it still names the network"
  [[ "$(docker inspect --format '{{.State.Running}}' e2e-on-server)" == true ]] || fail "removing the ingress stopped what isn't its own"

  step "Ingress: installing again with its data takes the ports back for the routes"
  start_dockwell
  wait_for_dockwell
  answer=""
  for _ in $(seq 300); do
    answer=$(plain_http -o /dev/null -w '%{http_code}' 2>/dev/null) || answer=""
    [[ "$answer" == 200 ]] && break
    sleep 0.2
  done
  [[ "$answer" == 200 ]] || fail "with Dockwell back, http://$behind/ answers '$answer' after 60 s"
else
  echo "Uninstalling wasn't run on real Docker, as that needs E2E_ROOT, and no Dockwell installed here. CI has both"
fi

dw routes delete through --yes || fail "routes delete through failed"
docker rm --force e2e-on-server >/dev/null || fail "can't remove e2e-on-server"
gone=$(dw routes delete behind --yes 2>&1) || fail "routes delete behind failed: $gone"
[[ "$gone" == *"so Caddy stops, and ports 80/tcp are free"* ]] || fail "the last plain route doesn't say what it frees: $gone"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "dockwell-ingress is there after the last route"; fi
if listening 80; then fail "port 80 is still held after the last route"; fi

# Moby takes a container's own no-new-privileges over the daemon's. It needs root,
# and a Docker that systemd runs, to restart it with the setting.
if [[ -n "${E2E_ROOT:-}" ]] && systemctl is-active --quiet docker 2>/dev/null; then
  step "Ingress: a Docker with no-new-privileges runs Caddy, and nothing else changes"
  daemon_before=$(priv cat /etc/docker/daemon.json 2>/dev/null) || daemon_before=""
  daemon_changed=1
  jq '. + {"no-new-privileges": true}' <<<"${daemon_before:-"{}"}" | priv tee /etc/docker/daemon.json >/dev/null || fail "can't change /etc/docker/daemon.json"
  priv systemctl restart docker || fail "can't restart Docker with no-new-privileges"
  options=$(docker info --format '{{json .SecurityOptions}}') || fail "Docker didn't come back with no-new-privileges"
  [[ "$options" == *name=no-new-privileges* ]] || fail "Docker doesn't say it sets no-new-privileges, so this proves nothing: $options"
  other=$(docker run --rm nginx:1.27-alpine grep NoNewPrivs /proc/self/status) || fail "can't run a container under no-new-privileges"
  [[ "$other" =~ NoNewPrivs:[[:space:]]+1 ]] || fail "a container on this daemon has $other, so this proves nothing"
  # The agent may not have seen Docker come back yet.
  started=""
  for _ in $(seq 30); do
    if made=$(dw routes create www --domain "$domain" --service web --port 80 --yes 2>&1); then
      started=1
      break
    fi
    sleep 2
  done
  [[ -n "$started" ]] || fail "on a Docker with no-new-privileges, a route failed: $made"
  [[ "$(docker inspect --format '{{json .HostConfig.SecurityOpt}}' dockwell-ingress)" == *no-new-privileges=false* ]] || fail "Caddy's container doesn't say no-new-privileges=false"
  caddy_says=$(docker exec dockwell-ingress grep NoNewPrivs /proc/1/status) || fail "can't ask Caddy's container"
  [[ "$caddy_says" =~ NoNewPrivs:[[:space:]]+0 ]] || fail "Caddy runs with $caddy_says, and would ignore its file capability"
  for _ in $(seq 100); do
    keeper=$(docker inspect --format '{{.State.Running}}' dockwell-ingress-keep 2>/dev/null) || keeper=""
    [[ "$keeper" == true ]] && break
    sleep 0.1
  done
  keeper_says=$(docker exec dockwell-ingress-keep grep NoNewPrivs /proc/1/status) || fail "can't ask the keeper"
  [[ "$keeper_says" =~ NoNewPrivs:[[:space:]]+1 ]] || fail "the keeper runs with $keeper_says, and needs nothing that takes off"
  for p in 80 443; do
    listening "$p" || fail "Caddy can't bind port $p on a Docker with no-new-privileges: $(docker logs --tail 20 dockwell-ingress 2>&1)"
  done
  for _ in $(seq 300); do
    over_https >/dev/null 2>&1 && break
    sleep 0.1
  done
  body=$(over_https) || fail "on a Docker with no-new-privileges, https://$domain/ doesn't answer"
  [[ "$body" == *nginx* ]] || fail "on a Docker with no-new-privileges, https://$domain/ doesn't serve web: $body"
  restore_daemon || fail "can't put Docker's settings back"
  removed=""
  for _ in $(seq 30); do
    if made=$(dw routes delete www --yes 2>&1); then
      removed=1
      break
    fi
    sleep 2
  done
  [[ -n "$removed" ]] || fail "routes delete www failed with Docker's settings back: $made"
else
  echo "Docker wasn't run with no-new-privileges, as that needs E2E_ROOT and systemd. CI has both"
fi

if [[ -n "${E2E_PANEL:-}" ]]; then
  step "Ingress: the panel's own domain, on its own ports"
  panel=panel.e2e-dockwell.example.net
  panel_url() { curl -fsS --max-time 10 --cacert "$pebble_dir/root.pem" --resolve "$panel:443:127.0.0.1" "$@"; }
  wait_for_panel() {
    for _ in $(seq 120); do
      [[ "$(panel_url "https://$panel/healthz" 2>/dev/null)" == ok ]] && return 0
      sleep 0.5
    done
    return 1
  }
  plain_status() { curl -sS --max-time 5 -o /dev/null -w '%{http_code}' --resolve "$panel:80:127.0.0.1" "$@" "http://$panel/"; }
  if [[ -z "${E2E_ROOT:-}" ]]; then
    sudo -n sysctl -q -w net.ipv4.ip_unprivileged_port_start=80 || fail "can't let this user bind ports 80 and 443, which the panel needs to hold them itself"
  fi
  stop_dockwell
  start_dockwell --domain "$panel"
  wait_for_dockwell
  wait_for_panel || fail "https://$panel/healthz didn't answer within 60 s, on the panel's own certificate"
  [[ "$(plain_status -H 'Authorization: Bearer x')" == 400 ]] || fail "a token over plain HTTP wasn't refused by the panel itself"
  if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress ran with no route"; fi

  step "Ingress: the first route moves the panel's domain behind Caddy, which only a person may do"
  set +e
  moved=$(dw routes create www --domain "$domain" --service web --port 80 --yes 2>&1)
  status=$?
  set -e
  [[ "$status" == 4 ]] || fail "moving the panel's domain behind Caddy exited $status, want 4, as a person approves it: $moved"
  grep -q "moves behind Caddy" <<<"$moved" || fail "the plan doesn't say the panel moves: $moved"
  grep -q "moves the panel's domain behind Caddy, so it needs a person" <<<"$moved" || fail "the CLI doesn't say why it needs a person: $moved"
  if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress started for a plan that waits for a person"; fi
  changes=$(dw changes list --json) || fail "changes list failed"
  move=$(jq -r '.changes[0].id' <<<"$changes")
  token_refused "$move"
  wait_for_panel || fail "https://$panel/healthz stopped answering on the panel's own certificate while the plan waited"
  person_apply "$move"
  says true docker inspect --format '{{.State.Running}}' dockwell-ingress || fail "dockwell-ingress isn't running"
  wait_for_panel || fail "https://$panel/healthz didn't answer through Caddy within 60 s"
  says nginx over_https || fail "with the panel beside it, https://$domain/ doesn't serve web"
  state=$(panel_url -X POST -H 'Content-Type: application/json' -d '{}' "https://$panel/dockwell.v1.AuthService/GetAuthState") ||
    fail "the panel's API doesn't answer through Caddy"
  [[ "$(jq -r .rpId <<<"$state")" == "$panel" ]] || fail "through Caddy, the panel's RP ID is not its domain: $state"
  [[ "$(plain_status -H 'Authorization: Bearer x')" == 400 ]] || fail "through Caddy, a token over plain HTTP wasn't refused"
  [[ "$(plain_status -X POST)" == 400 ]] || fail "through Caddy, a POST over plain HTTP wasn't refused"
  [[ "$(plain_status)" == 308 ]] || fail "through Caddy, a GET over plain HTTP wasn't redirected"
  [[ "$(priv stat -c '%a' "$data/ingress/run/panel.sock")" == 600 ]] || fail "the panel's socket has mode $(priv stat -c '%a' "$data/ingress/run/panel.sock"), want 600"
  [[ "$(priv stat -c '%u' "$data/ingress/run/panel.sock")" == "$caddy_uid" ]] || fail "the panel's socket is owned by uid $(priv stat -c '%u' "$data/ingress/run/panel.sock"), want $caddy_uid"

  step "Ingress: nothing on loopback can pretend to be HTTPS"
  spoofed=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' -H "Host: $panel" -H 'X-Forwarded-Proto: https' -H 'X-Forwarded-For: 203.0.113.9' "http://$addr/healthz")
  [[ "$spoofed" == 421 ]] || fail "the panel's Host on loopback, with forwarded headers, got $spoofed, want 421"

  step "Ingress: with Caddy down, the tunnel's listener still answers"
  docker stop --time 5 dockwell-ingress >/dev/null || fail "docker stop dockwell-ingress failed"
  [[ "$(curl -fsS --max-time 5 "http://$addr/healthz")" == ok ]] || fail "with Caddy down, the panel's loopback listener doesn't answer"
  # The agent starts it too, within a minute, so it may have done so already.
  docker start dockwell-ingress >/dev/null 2>&1 || true
  wait_for_panel || fail "https://$panel/healthz didn't come back with Caddy"

  step "Ingress: a restart of Dockwell leaves the ports to Caddy"
  stop_dockwell
  start_dockwell --domain "$panel"
  wait_for_dockwell
  grep -q 'hold ports 80 and 443' "$log" || fail "Dockwell didn't say the ports are Caddy's"
  wait_for_panel || fail "after a restart of Dockwell, https://$panel/healthz doesn't answer"

  step "Ingress: the last route gives the panel its ports back"
  back=$(dw routes delete www --yes 2>&1) || fail "routes delete www beside the panel failed: $back"
  grep -q "takes those ports back" <<<"$back" || fail "the plan doesn't say the panel takes its ports back: $back"
  if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "dockwell-ingress is there after the last route"; fi
  wait_for_panel || fail "https://$panel/healthz didn't answer on the panel's own ports again within 60 s"
  state=$(panel_url -X POST -H 'Content-Type: application/json' -d '{}' "https://$panel/dockwell.v1.AuthService/GetAuthState") ||
    fail "the panel's API doesn't answer on its own ports"
  [[ "$(jq -r .rpId <<<"$state")" == "$panel" ]] || fail "back on its own ports, the panel's RP ID is not its domain: $state"
  stop_dockwell
  if [[ -z "${E2E_ROOT:-}" ]]; then
    sudo -n sysctl -q -w net.ipv4.ip_unprivileged_port_start=1024 || true
  fi
  start_dockwell
  wait_for_dockwell
else
  echo "E2E_PANEL isn't set, so moving the panel's domain behind Caddy wasn't checked on real Docker. CI sets it"
fi

step "Ingress: a token can't take a domain another environment held"
dw environments create staging --yes || fail "environments create staging failed"
dw services create web --environment staging --app-image nginx:1.27-alpine --yes || fail "services create web in staging failed"
set +e
taken=$(dw routes create www --environment staging --domain "$domain" --service web --port 80 --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "taking a domain from another environment exited $status, want 4: it waits for a person: $taken"
grep -q 'the environment production held' <<<"$taken" || fail "the plan doesn't say whose domain it takes: $taken"
grep -q 'takes a domain another environment held, so it needs a person' <<<"$taken" || fail "the CLI doesn't say why it needs a person: $taken"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress started for a plan that waits for a person"; fi
changes=$(dw changes list --json) || fail "changes list failed"
takeover=$(jq -r '.changes[0].id' <<<"$changes")
# The CLI stops short of asking, so this is the server's own refusal.
token_refused "$takeover"
dw changes discard "$takeover" >/dev/null || fail "changes discard failed"
dw services delete web --environment staging --yes || fail "services delete web in staging failed"
dw environments delete staging --yes || fail "environments delete staging failed"

dw services delete web --yes || fail "services delete web failed"
in_ingress=""

step "A file in the shapes real ones have runs, as a person approves it"
real_port=$((web_port + 1))
docker network create e2e-edge >/dev/null || fail "docker network create e2e-edge failed"
dw environments create real --yes >/dev/null || fail "environments create real failed"
dw environments create other --yes >/dev/null || fail "environments create other failed"
cat >"$data/real.yaml" <<'EOF'
services:
  shop:
    container_name: e2e-storefront
    image: nginx:1.27-alpine
    build: ./not-read
    deploy:
      resources:
        limits:
          cpus: ${CPUS}
          memory: ${MEMORY_LIMIT}
        reservations:
          cpus: "0.25"
          memory: 16M
    cap_add: [NET_ADMIN]
    cap_drop: [MKNOD]
    group_add: ["12345"]
    labels:
      createdBy: Apps
    networks: [e2e-edge]
    ports:
      - ${HOST_IP}:${PANEL_APP_PORT_HTTP}:80
    restart: on-failure:5
    healthcheck:
      test: ["CMD-SHELL", "wget -q -O /dev/null http://127.0.0.1/ || exit 1"]
      interval: 2s
      timeout: 2s
      retries: 15
    volumes:
      - ./data:/data
networks:
  e2e-edge:
    external: true
EOF
printf 'CPUS=0.50\nMEMORY_LIMIT=64M\nHOST_IP=\nPANEL_APP_PORT_HTTP=%s\n' "$real_port" >"$data/real.env"
import_real() { dw compose import --compose "$(cat "$data/real.yaml")" --env-file "$data/real.env" --directory "$real_dir" --project default --environment real "$@"; }

planned=$(import_real --plan-only --json) || fail "importing the file failed"
change=$(jq -r '.change.id // empty' <<<"$planned") || fail "compose import printed what isn't JSON: $planned"
[[ "$change" == chg_* ]] || fail "compose import planned no change: $planned"
jq -e '.change.plan.hostAccess == true' <<<"$planned" >/dev/null || fail "the file plans no host access: $planned"
for says in 'e2e-edge' 'NET_ADMIN' '12345' "$real_dir/data"; do
  [[ "$planned" == *"$says"* ]] || fail "the plan doesn't name $says: $planned"
done
for reason in container_name no_build cpu_reservation; do
  jq -e --arg r "$reason" 'any(.change.plan.notes[]; .reason == $r)' <<<"$planned" >/dev/null || fail "the plan doesn't say $reason: $planned"
done
token_refused "$change"
person_apply "$change"

shop=$(docker ps --filter label=com.docker.compose.service=shop --format '{{.Names}}') || fail "docker ps failed"
[[ "$shop" == env_*-shop-1 ]] || fail "Docker runs '$shop' for shop, want env_…-shop-1, not the file's container_name"
inspected=$(docker inspect "$shop") || fail "docker inspect $shop failed"
real_projects+=("$(jq -r '.[0].Config.Labels["com.docker.compose.project"]' <<<"$inspected")")
has() { jq -e "$1" <<<"$inspected" >/dev/null || fail "$shop isn't as asked: $1 is $(jq -c "$1" <<<"$inspected"), in $(jq -c '.[0].HostConfig' <<<"$inspected")"; }
has '.[0].HostConfig.NanoCpus == 500000000'
has '.[0].HostConfig.Memory == 67108864'
has '.[0].HostConfig.MemoryReservation == 16777216'
has '.[0].HostConfig.CapAdd | any(endswith("NET_ADMIN"))'
has '.[0].HostConfig.CapDrop | any(endswith("MKNOD"))'
has '.[0].HostConfig.GroupAdd | index("12345") != null'
has '.[0].HostConfig.RestartPolicy.Name == "on-failure" and .[0].HostConfig.RestartPolicy.MaximumRetryCount == 5'
has '.[0].NetworkSettings.Networks["e2e-edge"].Aliases | (index("e2e-storefront") != null and index("shop") != null)'
has '.[0].NetworkSettings.Networks | keys == ["e2e-edge"]'
has '.[0].HostConfig.Binds | length == 1'
bind=$(jq -r '.[0].HostConfig.Binds[0]' <<<"$inspected")
[[ "$bind" == "$real_dir/data:/data" ]] || fail "./data mounts as '$bind', want $real_dir/data:/data"
page=$(curl -fsS "http://127.0.0.1:$real_port/") || fail "shop doesn't serve on port $real_port, on the empty HOST_IP's every address"
[[ "$page" == *nginx* ]] || fail "shop serves something other than nginx on port $real_port: ${page:0:300}"
drift=$(dw drift list --json) || fail "drift list failed"
# Another environment has drift of its own by now, as an ingress step above prunes
# a network it left unused, so this asks about shop alone.
jq -e 'any((.drift // [])[]; .resource.name == "shop") | not' <<<"$drift" >/dev/null || fail "drift with shop running as asked: $drift"

step "A limit changes in place, on a token, and the file's container stays"
before=$(jq -r '.[0].Id' <<<"$inspected")
import_real --variables MEMORY_LIMIT=128M --yes >/dev/null || fail "changing the limit failed"
inspected=$(docker inspect "$shop") || fail "docker inspect $shop failed"
[[ "$(jq -r '.[0].Id' <<<"$inspected")" == "$before" ]] || fail "changing a limit recreated $shop"
has '.[0].HostConfig.Memory == 134217728'

step "It exports as a file docker compose reads, naming the network as external"
dw compose export --environment real >"$data/real-export.yaml" || fail "compose export failed"
grep -q 'external: true' "$data/real-export.yaml" || fail "the export doesn't name its network as external:
$(cat "$data/real-export.yaml")"
docker compose -f "$data/real-export.yaml" config --quiet || fail "docker compose doesn't take the export:
$(cat "$data/real-export.yaml")"
listed=$(docker compose -f "$data/real-export.yaml" ps --format '{{.Service}}') || fail "docker compose -f <export> ps failed"
[[ "$listed" == shop ]] || fail "docker compose -f <export> ps lists '$listed', want shop"

step "Another project on the network reaches it by name, and can't take that name"
docker tag nginx:1.27-alpine e2e-local-only:1 || fail "docker tag failed"
other_file=$(printf 'services:\n  client:\n    image: e2e-local-only:1\n    build: .\n    networks: [e2e-edge]\nnetworks:\n  e2e-edge:\n    external: true\n')
planned=$(dw compose import --compose "$other_file" --project default --environment other --plan-only --json) || fail "importing a local-only image failed"
change=$(jq -r '.change.id // empty' <<<"$planned") || fail "compose import printed what isn't JSON: $planned"
token_refused "$change"
person_apply "$change"
client=$(docker ps --filter label=com.docker.compose.service=client --format '{{.Names}}') || fail "docker ps failed"
[[ -n "$client" ]] || fail "Docker runs nothing for client"
real_projects+=("$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$client")")
page=$(docker exec "$client" wget -q -O - -T 5 http://e2e-storefront/) || fail "client can't reach shop by its container_name on e2e-edge"
[[ "$page" == *nginx* ]] || fail "client reached something other than shop: ${page:0:300}"
page=$(docker exec "$shop" wget -q -O - -T 5 http://client/) || fail "shop can't reach client by its name on e2e-edge"
[[ "$page" == *nginx* ]] || fail "shop reached something other than client: ${page:0:300}"
clash=$(printf 'services:\n  clash:\n    image: nginx:1.27-alpine\n    container_name: e2e-storefront\n    networks: [e2e-edge]\nnetworks:\n  e2e-edge:\n    external: true\n')
if out=$(dw compose import --compose "$clash" --project default --environment other --plan-only 2>&1); then
  fail "a second container answering to e2e-storefront on e2e-edge was planned: ${out:0:300}"
fi
[[ "$out" == *"already does"* ]] || fail "a name another has failed some other way: ${out:0:300}"

step "What isn't on the server is refused as it plans, by name"
missing=$(printf 'services:\n  gone:\n    image: nginx:1.27-alpine\n    networks: [e2e-missing]\nnetworks:\n  e2e-missing:\n    external: true\n')
if out=$(dw compose import --compose "$missing" --project default --environment other --plan-only 2>&1); then
  fail "a network the server hasn't got was planned: ${out:0:300}"
fi
[[ "$out" == *"the network e2e-missing isn't on this server"* ]] || fail "a missing network failed some other way: ${out:0:300}"
nothing=$(printf 'services:\n  gone:\n    image: dockwell-e2e-no-such-image:1\n    build: .\n')
if out=$(dw compose import --compose "$nothing" --project default --environment other --plan-only 2>&1); then
  fail "an image no one has was planned: ${out:0:300}"
fi
[[ "$out" == *"won't give it to a pull"* ]] || fail "an image no one has failed some other way: ${out:0:300}"

step "Going leaves the network as it was"
dw changes undo "$change" --yes >/dev/null || fail "changes undo $change failed"
if docker inspect "$client" >/dev/null 2>&1; then fail "the undo left $client"; fi
dw services delete shop --environment real --yes >/dev/null || fail "services delete shop failed"
if docker inspect "$shop" >/dev/null 2>&1; then fail "deleting shop left $shop"; fi
docker network inspect e2e-edge >/dev/null || fail "the undo or the deletion took the network e2e-edge, which Dockwell didn't make"

# Last of what names a project: its projects stay, as nothing deletes one, and
# with more than one, everything that names none is refused for not saying which.
step "Two Compose files with a service of one name run side by side"
# With nothing to say where it goes, a file is refused now the default project
# has what it would change.
dw services create keeper --environment production --app-image nginx:1.27-alpine --yes || fail "services create keeper failed"
if out=$(dw compose import --compose "services: {keeper: {image: nginx:1.28-alpine}}" --environment production --yes 2>&1); then
  fail "a file with no name was imported into a project with apps: $out"
fi
grep -q "names no project" <<<"$out" || fail "a file with no name was refused saying: $out"
named_file() { printf 'name: %s\nservices:\n  db:\n    image: %s\n' "$1" "$2"; }
dw compose import --compose "$(named_file blog nginx:1.27-alpine)" --yes || fail "importing blog failed"
dw compose import --compose "$(named_file wiki nginx:1.28-alpine)" --yes || fail "importing wiki failed"
dbs=$(docker ps --filter label=com.docker.compose.service=db --format '{{.Names}} {{.Image}} {{.Networks}}') || fail "docker ps failed"
[[ "$(wc -l <<<"$dbs")" == 2 ]] || fail "Docker runs these dbs, want two: $dbs"
project_of() { docker ps --filter label=com.docker.compose.service=db --filter "ancestor=$1" --format '{{.Label "com.docker.compose.project"}}'; }
blog_project=$(project_of nginx:1.27-alpine) || fail "docker ps failed"
wiki_project=$(project_of nginx:1.28-alpine) || fail "docker ps failed"
extra_projects="$blog_project $wiki_project"
[[ -n "$blog_project" && -n "$wiki_project" && "$blog_project" != "$wiki_project" ]] ||
  fail "the two files' dbs are in projects '$blog_project' and '$wiki_project', want one each"
members=$(docker network inspect --format '{{range .Containers}}{{.Name}} {{end}}' "${blog_project}_default") ||
  fail "blog has no network of its own"
[[ "$members" != *"$wiki_project"* ]] || fail "wiki's db is on blog's network: $members"
docker network inspect "${wiki_project}_default" >/dev/null || fail "wiki has no network of its own"
listed=$(docker compose -p "$blog_project" ps --format '{{.Service}}') || fail "docker compose -p $blog_project ps failed"
[[ "$listed" == db ]] || fail "docker compose lists '$listed' in blog's project, want db"
# Naming one again updates it, and doesn't touch the other.
wiki_state() { docker inspect --format '{{.Id}} {{.Config.Image}} {{.State.StartedAt}}' "${wiki_project}-db-1"; }
wiki_before=$(wiki_state) || fail "wiki's db isn't called ${wiki_project}-db-1"
dw compose import --compose "$(named_file blog nginx:1.28-alpine)" --yes || fail "importing blog again failed"
again=$(docker ps --filter "label=com.docker.compose.project=$blog_project" --format '{{.Image}}') || fail "docker ps failed"
[[ "$again" == nginx:1.28-alpine ]] || fail "after importing blog again, its db runs '$again'"
wiki_after=$(wiki_state) || fail "after importing blog again, wiki's db is gone"
[[ "$wiki_after" == "$wiki_before" ]] || fail "importing blog again touched wiki's db: $wiki_before, then $wiki_after"
dw services delete db --project blog --yes || fail "deleting blog's db failed"
dw services delete db --project wiki --yes || fail "deleting wiki's db failed"
dw services delete keeper --project default --environment production --yes || fail "deleting keeper failed"

step "The agent build joins, and runs a container"
made=$(dw token create 2>>"$log") || fail "dockwell token create failed"
ticket=$(awk '$1 == "dockwell" && $2 == "join" { print $3 }' <<<"$made")
[[ -n "$ticket" ]] || fail "dockwell token create printed no ticket: $made"
"$agent" join --data-dir "$agent_data" --name e2e-agent "$ticket" >"$agent_log" 2>&1 || fail "the agent build didn't join"
"$agent" run --data-dir "$agent_data" --log-format json >"$agent_log" 2>&1 &
agent_pid=$!
joined() {
  local lines
  lines=$(grep '"msg":"server connected"' "$log" | grep '"name":"e2e-agent"' | grep '"build":"agent"') || return 1
  [[ "$lines" == *'"container"'* ]]
}
for _ in $(seq 300); do
  joined && break
  sleep 0.1
done
joined || fail "the agent build didn't connect within 30 s, offering the Docker kinds"
dw containers create e2e-agent-web --on e2e-agent --image nginx:1.27-alpine --yes || fail "containers create on the agent build failed"
owner=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' e2e-agent-web) ||
  fail "Docker runs no container e2e-agent-web"
[[ "$owner" == srv_* ]] || fail "e2e-agent-web is in the Compose project $owner, want its server's, srv_…"
running=$(docker inspect --format '{{.State.Running}}' e2e-agent-web) || fail "docker inspect e2e-agent-web failed"
[[ "$running" == true ]] || fail "e2e-agent-web isn't running"
dw containers delete e2e-agent-web --on e2e-agent --yes || fail "containers delete on the agent build failed"
if docker inspect e2e-agent-web >/dev/null 2>&1; then fail "e2e-agent-web is left after deleting it"; fi

echo "docker-e2e: ok"
