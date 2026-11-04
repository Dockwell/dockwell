#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Runs built dockwell binaries against this machine's Docker Engine, end to
# end through the CLI: a service from an image runs, changes and is undone;
# the environment exports as a Compose file that docker compose reads and
# lists; a Compose file imports; a service using a variable and a secret
# runs with their values, exports without the secret, and imports again
# with no change; and everything goes again. Then the agent build joins the full build's control plane, and runs a container on the
# same Docker. Before that, a route gets a real certificate from Pebble, Let's
# Encrypt's test CA, and serves a real container over HTTPS through Caddy. It
# uses throwaway data directories and removes what it made, so it never touches
# a real install. It needs Docker with the compose plugin, Go, curl, jq, openssl
# and iproute2's ss, a free port (E2E_WEB_PORT, 18080 by default), and ports 80
# and 443 free, for the ingress's own part. With E2E_PANEL set, as in CI, it also
# moves the panel's own domain behind Caddy and back, which needs sudo to let
# this user bind ports 80 and 443.
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
agent_pid=""

pebble_dir=$(mktemp -d)
tools=${E2E_INGRESS_TOOLS:-$(mktemp -d)}
pebble_pids=()

# The ingress's certificates come from Pebble, on this machine.
export DOCKWELL_ACME_DIRECTORY_URL=https://localhost:14000/dir
export DOCKWELL_ACME_ROOTS=$pebble_dir/cert.pem
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 -subj /CN=localhost \
  -addext subjectAltName=DNS:localhost,IP:127.0.0.1 -keyout "$pebble_dir/key.pem" -out "$DOCKWELL_ACME_ROOTS" 2>/dev/null

start_dockwell() {
  # Only the panel's own certificates need this, as they come through Go's
  # default roots.
  SSL_CERT_FILE=$DOCKWELL_ACME_ROOTS "$bin" run --listen "$addr" --agent-listen 127.0.0.1:0 --data-dir "$data" --log-format json "$@" >>"$log" 2>&1 &
  pid=$!
}
start_dockwell
cleanup() {
  if [[ -n "$project" ]]; then
    docker compose -p "$project" down --volumes --remove-orphans --timeout 1 >/dev/null 2>&1 || true
  fi
  docker rm --force e2e-agent-web e2e-holds-80 dockwell-ingress >/dev/null 2>&1 || true
  docker network rm dockwell_ingress >/dev/null 2>&1 || true
  [[ -z "$agent_pid" ]] || kill "$agent_pid" 2>/dev/null || true
  kill "$pid" 2>/dev/null || true
  for p in "${pebble_pids[@]:-}"; do
    [[ -z "$p" ]] || kill "$p" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  rm -rf "$log" "$agent_log" "$data" "$agent_data" "$pebble_dir"
}
trap cleanup EXIT

# ingress_state says all there is to know of the ingress, as most of what can
# go wrong with it is on real Docker, where nobody can run it but CI.
ingress_state() {
  echo "--- the ingress's container" >&2
  docker ps -a --filter name=dockwell-ingress --format '{{.Names}} {{.Image}} {{.Status}} {{.Ports}}' >&2 || true
  docker inspect --format 'network {{.HostConfig.NetworkMode}}, user {{.Config.User}}, restart {{.HostConfig.RestartPolicy.Name}}, exit {{.State.ExitCode}}, error {{.State.Error}}' dockwell-ingress >&2 2>/dev/null || true
  echo "--- its log" >&2
  docker logs --tail 60 dockwell-ingress >&2 2>&1 || true
  echo "--- the ingress network" >&2
  docker network inspect --format '{{.Name}} internal={{.Internal}} {{json .IPAM.Config}} {{json .Containers}} {{json .Options}}' dockwell_ingress >&2 2>/dev/null || true
  echo "--- what certificates are doing" >&2
  tail -n 40 "$data/ingress/data/dockwell-tls.log" >&2 2>/dev/null || true
  echo "--- addresses" >&2
  cat "$data/ingress/addresses.json" >&2 2>/dev/null || true
  echo "--- who is on ports 80 and 443" >&2
  ss -ltnup 'sport = :80 or sport = :443' >&2 2>/dev/null || true
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
export DOCKWELL_DATA_DIR=$data DOCKWELL_LOG_FORMAT=text
dw() { "$bin" "$@"; }
step() { echo "--- $*"; }

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
(cd "$(dirname "$0")/.." && go run ./scripts/claim "$link") || fail "can't claim the install"
dw login --local >/dev/null || fail "dockwell login --local failed"

ports="{\"hostPort\":$web_port,\"containerPort\":80,\"hostIp\":\"127.0.0.1\"}"

step "A service from an image runs"
dw services create web --app-image nginx:1.27-alpine --app-ports "$ports" --app-env GREETING=hello --yes ||
  fail "services create failed"
container=$(docker ps --filter label=com.docker.compose.service=web --format '{{.Names}}') || fail "docker ps failed"
[[ "$container" == env_*-web-1 ]] || fail "Docker runs '$container' for web, want env_…-web-1"
project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$container") ||
  fail "docker inspect $container failed"
[[ "$project" == env_* ]] || fail "$container is in the Compose project '$project', want its environment's, env_…"
curl -fsS "http://127.0.0.1:$web_port/" | grep -q nginx || fail "web doesn't serve on port $web_port"
env_of() { docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container"; }
env_of | grep -qx GREETING=hello || fail "web's environment is $(env_of)"

step "It changes, and the change undoes"
dw services update web --app-image nginx:1.27-alpine --app-ports "$ports" --app-env GREETING=hi --yes ||
  fail "services update failed"
env_of | grep -qx GREETING=hi || fail "after the update, web's environment is $(env_of)"
# Changes are listed newest first.
changes=$(dw changes list --json) || fail "changes list failed"
change=$(jq -r '.changes[0].id // empty' <<<"$changes") || fail "changes list printed what isn't JSON: $changes"
[[ "$change" == chg_* ]] || fail "changes list has no change: $changes"
dw changes undo "$change" --yes || fail "changes undo $change failed"
env_of | grep -qx GREETING=hello || fail "after the undo, web's environment is $(env_of)"
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

dw compose import --compose "$(cat "$data/stack.yaml")" --yes || fail "compose import failed"
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
stop_dockwell() {
  kill "$pid"
  wait "$pid" 2>/dev/null || true
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
dw routes create www --domain "$domain" --service web --port 80 --yes || fail "routes create www failed"
[[ "$(docker inspect --format '{{.Id}}' "$web")" == "$web_id" ]] || fail "adding a route made the service's container again, which is downtime"
hardening=$(docker inspect --format '{{.HostConfig.NetworkMode}} {{.HostConfig.ReadonlyRootfs}} {{.HostConfig.RestartPolicy.Name}}' dockwell-ingress) ||
  fail "there's no dockwell-ingress after the first route"
[[ "$hardening" == "host true unless-stopped" ]] || fail "dockwell-ingress runs as: $hardening, want the host network, a read-only root, and unless-stopped"
capabilities=$(docker inspect --format '{{.HostConfig.CapDrop}} {{.HostConfig.CapAdd}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
[[ "$capabilities" == *ALL*NET_BIND_SERVICE* ]] || fail "dockwell-ingress has the capabilities $capabilities, want all dropped but NET_BIND_SERVICE"
image=$(docker inspect --format '{{.Config.Image}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
[[ "$image" == caddy@sha256:* ]] || fail "dockwell-ingress runs $image, want it by digest"
user=$(docker inspect --format '{{.Config.User}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
[[ "$user" == "$(id -u):$(id -g)" ]] || fail "dockwell-ingress runs as $user, want this agent's own user, $(id -u):$(id -g)"
docker inspect --format '{{.State.Running}}' dockwell-ingress | grep -q true || fail "dockwell-ingress isn't running"
for p in 80 443; do
  listening "$p" || fail "nothing listens on port $p"
done
[[ -n "$(ss -H -lun 'sport = :443')" ]] || fail "nothing listens on UDP 443, for HTTP/3"
curl -fsS --max-time 2 http://127.0.0.1:2019/config/ >/dev/null 2>&1 && fail "Caddy's admin API answers on TCP"
[[ -S "$data/ingress/run/admin.sock" ]] || fail "there's no admin socket at $data/ingress/run/admin.sock"
[[ "$(stat -c '%a' "$data/ingress/run")" == 700 ]] || fail "the admin socket's directory has mode $(stat -c '%a' "$data/ingress/run"), want 700"

step "Ingress: the network is kept to itself"
network=$(docker network inspect --format '{{.Internal}} {{index .Options "com.docker.network.bridge.enable_icc"}}' dockwell_ingress) ||
  fail "there's no network dockwell_ingress"
[[ "$network" == "true false" ]] || fail "dockwell_ingress is internal and enable_icc: $network, want true and false"
address=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAddress}}' "$web") || fail "web isn't on dockwell_ingress"
static=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAMConfig.IPv4Address}}' "$web") || fail "web has no static address on dockwell_ingress"
[[ "$address" == "$static" ]] || fail "web is at $address, and asked for $static"
curl -fsS --max-time 5 "http://$address/" | grep -q nginx || fail "this machine can't reach web on the ingress network, at $address, as Caddy must"
if docker run --rm --network dockwell_ingress nginx:1.27-alpine wget -q -T 3 -O- "http://$address/" >/dev/null 2>&1; then
  fail "a container on the ingress network reached web: enable_icc: false isn't keeping them apart"
fi
if docker run --rm --network dockwell_ingress nginx:1.27-alpine wget -q -T 3 -O- http://1.1.1.1/ >/dev/null 2>&1; then
  fail "a container on the ingress network got out through it: internal: true isn't holding"
fi
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
over_https | grep -q nginx || fail "https://$domain/ doesn't serve web through Caddy: $(over_https -v 2>&1)"
redirect=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code} %{redirect_url}' --resolve "$domain:80:127.0.0.1" "http://$domain/x?y=1") || fail "http://$domain/ didn't answer"
[[ "$redirect" == "308 https://$domain/x?y=1" ]] || fail "http://$domain/x?y=1 answers $redirect, want a redirect to HTTPS"

step "Ingress: the service reads the real client, and Caddy owns the header"
over_https -H 'X-Forwarded-For: 6.6.6.6' >/dev/null || fail "the request with a forged header failed"
sleep 0.5
docker logs --tail 5 "$web" 2>&1 | tail -1 | grep -q '"127.0.0.1"$' ||
  fail "web's last request doesn't read as from 127.0.0.1, in X-Forwarded-For: $(docker logs --tail 5 "$web" 2>&1)"
if docker logs --tail 5 "$web" 2>&1 | grep -q '6\.6\.6\.6'; then fail "a forged X-Forwarded-For reached web"; fi
if curl -fsS --max-time 2 --cacert "$pebble_dir/root.pem" --resolve "$domain:443:[::1]" -o /dev/null "https://$domain/" 2>/dev/null; then
  sleep 0.5
  docker logs --tail 5 "$web" 2>&1 | tail -1 | grep -q '"::1"$' ||
    fail "over IPv6, web's last request doesn't read as from ::1, so a client's real address is lost: $(docker logs --tail 5 "$web" 2>&1)"
else
  echo "This machine has no IPv6 on loopback, so the client's real address over IPv6 wasn't checked"
fi

step "Ingress: a replaced service keeps its address, and goes on serving"
dw services update web --app-image nginx:1.27-alpine --app-env GREETING=hi --yes || fail "services update web failed"
[[ "$(docker inspect --format '{{.Id}}' "$web")" != "$web_id" ]] || fail "the update didn't replace web's container"
now=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAMConfig.IPv4Address}}' "$web") || fail "the new container isn't on dockwell_ingress"
[[ "$now" == "$static" ]] || fail "the new container is at $now, want the instance's address, $static"
over_https | grep -q nginx || fail "after the update, https://$domain/ doesn't serve web"
listed=$(docker compose -p "$project" ps --format '{{.Service}}') || fail "docker compose -p $project ps failed"
grep -qx web <<<"$listed" || fail "docker compose doesn't list web on the ingress network: $listed"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after a route and a replace: $drift"

step "Ingress: traffic keeps flowing with Dockwell down, and through a restart of Caddy"
stop_dockwell
over_https | grep -q nginx || fail "with Dockwell down, https://$domain/ stopped serving"
docker restart --time 5 dockwell-ingress >/dev/null || fail "docker restart dockwell-ingress failed"
for _ in $(seq 100); do
  over_https >/dev/null 2>&1 && break
  sleep 0.1
done
over_https | grep -q nginx || fail "with Dockwell down, Caddy came back without its last config"
start_dockwell
wait_for_dockwell

step "Ingress: a hand edit reads as drift, and a revert puts it back"
hand='{"handler":"static_response","body":"by hand"}'
curl -fsS --unix-socket "$data/ingress/run/admin.sock" -X PUT -H 'Content-Type: application/json' -d "$hand" \
  "http://caddy/id/dockwell.$project.www/handle/0" >/dev/null || fail "can't edit the route by hand"
over_https | grep -q 'by hand' || fail "the hand edit didn't take"
drifted=""
for _ in $(seq 60); do
  drift=$(dw drift list --json 2>/dev/null) || drift='{}'
  drifted=$(jq -r '.drift[]? | select(.resource.kind == "route") | .resourceId' <<<"$drift")
  [[ -n "$drifted" ]] && break
  sleep 0.5
done
[[ -n "$drifted" ]] || fail "a hand edit to the route doesn't read as drift: $drift"
over_https | grep -q 'by hand' || fail "Dockwell reverted a hand edit on its own: drift is reported, never silently reverted"
server=$(jq -r '.drift[] | select(.resource.kind == "route") | .serverId' <<<"$drift")
dw drift revert --server-id "$server" --resource-id "$drifted" --yes || fail "drift revert failed"
over_https | grep -q nginx || fail "after reverting, https://$domain/ doesn't serve web"

step "Ingress: the last route stops Caddy and frees the ports, and its certificate stays"
certificate=$(find "$data/ingress/data/caddy/certificates" -name "$domain.crt") || fail "can't look for the certificate"
[[ -n "$certificate" ]] || fail "there's no certificate on disk for $domain"
before=$(openssl x509 -in "$certificate" -noout -serial) || fail "can't read the certificate"
dw routes delete www --yes || fail "routes delete www failed"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "dockwell-ingress is there after the last route"; fi
if docker network inspect dockwell_ingress >/dev/null 2>&1; then fail "the network dockwell_ingress is there after the last route"; fi
for p in 80 443; do
  if listening "$p"; then fail "port $p is still held after the last route"; fi
done
[[ -f "$certificate" ]] || fail "the certificate went with Caddy"
[[ -d "$data/ingress/data/caddy/acme" ]] || fail "the ACME account went with Caddy"
if docker inspect --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$web" | grep -q dockwell_ingress; then fail "web stayed on the ingress network"; fi

step "Ingress: a route again reuses its certificate"
dw routes create www --domain "$domain" --service web --port 80 --yes || fail "routes create www, again, failed"
for _ in $(seq 100); do
  over_https >/dev/null 2>&1 && break
  sleep 0.1
done
over_https | grep -q nginx || fail "with the route back, https://$domain/ doesn't serve web"
after=$(openssl x509 -in "$certificate" -noout -serial) || fail "can't read the certificate"
[[ "$after" == "$before" ]] || fail "a new certificate was issued, $after, where the one kept was $before"
dw routes delete www --yes || fail "routes delete www, again, failed"

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
  sudo -n sysctl -q -w net.ipv4.ip_unprivileged_port_start=80 || fail "can't let this user bind ports 80 and 443, which the panel needs to hold them itself"
  stop_dockwell
  start_dockwell --domain "$panel"
  wait_for_dockwell
  wait_for_panel || fail "https://$panel/healthz didn't answer within 60 s, on the panel's own certificate"
  [[ "$(plain_status -H 'Authorization: Bearer x')" == 400 ]] || fail "a token over plain HTTP wasn't refused by the panel itself"
  if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress ran with no route"; fi

  step "Ingress: the first route moves the panel's domain behind Caddy"
  moved=$(dw routes create www --domain "$domain" --service web --port 80 --yes 2>&1) || fail "routes create www beside the panel failed: $moved"
  grep -q "moves behind Caddy" <<<"$moved" || fail "the plan doesn't say the panel moves: $moved"
  docker inspect --format '{{.State.Running}}' dockwell-ingress | grep -q true || fail "dockwell-ingress isn't running"
  wait_for_panel || fail "https://$panel/healthz didn't answer through Caddy within 60 s"
  over_https | grep -q nginx || fail "with the panel beside it, https://$domain/ doesn't serve web"
  state=$(panel_url -X POST -H 'Content-Type: application/json' -d '{}' "https://$panel/dockwell.v1.AuthService/GetAuthState") ||
    fail "the panel's API doesn't answer through Caddy"
  [[ "$(jq -r .rpId <<<"$state")" == "$panel" ]] || fail "through Caddy, the panel's RP ID is not its domain: $state"
  [[ "$(plain_status -H 'Authorization: Bearer x')" == 400 ]] || fail "through Caddy, a token over plain HTTP wasn't refused"
  [[ "$(plain_status -X POST)" == 400 ]] || fail "through Caddy, a POST over plain HTTP wasn't refused"
  [[ "$(plain_status)" == 308 ]] || fail "through Caddy, a GET over plain HTTP wasn't redirected"
  [[ "$(stat -c '%a' "$data/ingress/run/panel.sock")" == 600 ]] || fail "the panel's socket has mode $(stat -c '%a' "$data/ingress/run/panel.sock"), want 600"

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
  sudo -n sysctl -q -w net.ipv4.ip_unprivileged_port_start=1024 || true
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
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress started for a plan that waits for a person"; fi
changes=$(dw changes list --json) || fail "changes list failed"
dw changes discard "$(jq -r '.changes[0].id' <<<"$changes")" >/dev/null || fail "changes discard failed"
dw services delete web --environment staging --yes || fail "services delete web in staging failed"
dw environments delete staging --yes || fail "environments delete staging failed"

dw services delete web --yes || fail "services delete web failed"
in_ingress=""

step "The agent build joins, and runs a container"
made=$(dw token create 2>>"$log") || fail "dockwell token create failed"
ticket=$(awk '$1 == "dockwell" && $2 == "join" { print $3 }' <<<"$made")
[[ -n "$ticket" ]] || fail "dockwell token create printed no ticket: $made"
"$agent" join --data-dir "$agent_data" --name e2e-agent "$ticket" >"$agent_log" 2>&1 || fail "the agent build didn't join"
"$agent" run --data-dir "$agent_data" --log-format json >"$agent_log" 2>&1 &
agent_pid=$!
joined() { grep '"msg":"server connected"' "$log" | grep '"name":"e2e-agent"' | grep '"build":"agent"' | grep -q '"container"'; }
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
