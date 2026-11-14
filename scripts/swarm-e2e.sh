#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Makes what Dokploy runs as Swarm services again in Dockwell, on this machine's
# real single-node Swarm (ADR 0089, "Swarm-run apps"), through a person's plan, and
# undoes it. Dokploy is set up as its installer and its deploys leave it: the
# overlay network dokploy-network, its Traefik on that network routing a domain
# to an application by its appName from a file in its dynamic folder, the
# application as a Swarm service publishing a port through the routing mesh, with
# a volume of what it serves, and a stack with PostgreSQL holding a row. Then:
#
#   - the import plans making the application and the stack again, refusing none
#     of what a real Swarm task carries;
#   - each old task is gone before its copy starts, so two PostgreSQL never run on
#     one data directory;
#   - Traefik still reaches the application by its appName on dokploy-network, and
#     another container there reaches the stack's service by its Swarm name;
#   - each copy restarts as its Swarm service did;
#   - the port the service published is freed, and the copy publishes it;
#   - what can't come stays behind, each with its reason: more than one replica,
#     a global service, a Swarm secret, an anonymous volume;
#   - undoing it puts each Swarm service back as it was, ports and all.
#
# It needs Docker in Swarm mode, as `docker swarm init` leaves it, Go, curl and jq,
# and sudo, as Dockwell runs as root, as on a server. It removes what it made.
#
#   scripts/swarm-e2e.sh bin/dockwell
set -euo pipefail
# Anything this starts asks nothing beyond this machine (DOCKWELL_TEST, ADR 0034).
export DOCKWELL_TEST=1

usage="usage: scripts/swarm-e2e.sh path/to/dockwell"
for tool in docker curl go jq; do
  command -v "$tool" >/dev/null || {
    echo "swarm-e2e: $tool isn't installed, and the script needs it. Install it, then run it again" >&2
    exit 1
  }
done
bin=$(realpath "${1:?$usage}")
[[ "$(docker info --format '{{.Swarm.LocalNodeState}}')" == active ]] || {
  echo "swarm-e2e: Docker isn't in Swarm mode. Run docker swarm init, then run it again" >&2
  exit 1
}

addr=127.0.0.1:${E2E_PORT:-7698}
port=${addr##*:}
data=$(mktemp -d)
log=$(mktemp)
cookie=$(mktemp)
passkey=$(mktemp)
stepup=$(mktemp)
# A server's root as the agent reads it for Dokploy's folders (DOCKWELL_TEST_PANELS).
root=$(mktemp -d)
dynamic=$root/etc/dokploy/traefik/dynamic
traefik_port=${E2E_TRAEFIK_PORT:-18088}
nginx=nginx:1.27-alpine
postgres=postgres:17.11@sha256:748a18a54d3842d43bc156f0840d78029b8d349e0feca98c12b9c4ce3404a6b3
traefik=traefik:v3.1
pid=""

priv() { sudo -n "$@"; }
dw() { priv env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text "$bin" "$@"; }
step() { echo "--- $*"; }

cleanup() {
  for s in shop-k2m4 many-r2 everywhere-g1 keeps-s1 anon-a1; do
    docker service rm "$s" >/dev/null 2>&1 || true
  done
  docker stack rm forum-q1 >/dev/null 2>&1 || true
  docker rm --force dokploy-traefik >/dev/null 2>&1 || true
  # Dockwell's copies, by the projects the import made.
  docker ps --all --format '{{.Names}}' | grep -E '^(store|forum)\.' | xargs --no-run-if-empty docker rm --force >/dev/null 2>&1 || true
  [[ -z "$pid" ]] || priv kill "$pid" 2>/dev/null || true
  wait 2>/dev/null || true
  # The stack's tasks take a moment to go, and the network and volumes with them.
  for _ in $(seq 30); do
    [[ -z "$(docker ps --all --quiet --filter label=com.docker.stack.namespace=forum-q1)" ]] && break
    sleep 1
  done
  docker secret rm e2e-swarm-secret >/dev/null 2>&1 || true
  docker network rm dokploy-network >/dev/null 2>&1 || true
  docker volume rm shop-data forum-q1_pgdata >/dev/null 2>&1 || true
  priv rm -rf "$data" "$log" "$cookie" "$passkey" "$stepup" "$root"
}
trap cleanup EXIT

fail() {
  echo "swarm-e2e: $*" >&2
  echo "--- Swarm's services" >&2
  docker service ls >&2 || true
  echo "--- containers" >&2
  docker ps --all --format '{{.Names}} {{.Image}} {{.Status}} {{.Ports}}' >&2 || true
  echo "--- dockwell log" >&2
  priv cat "$log" >&2 || true
  exit 1
}

# replicas is how many replicas a Swarm service asks for.
replicas() { docker service inspect --format '{{.Spec.Mode.Replicated.Replicas}}' "$1"; }
# running_tasks lists the containers that run for a Swarm service on this node.
running_tasks() { docker ps --quiet --filter "label=com.docker.swarm.service.name=$1"; }
# copy_of names Dockwell's container of a service, in its project, which Dockwell
# names <project>.<environment>.<service>-1.
copy_of() { docker ps --all --format '{{.Names}}' | grep -E "^$1\.[a-z0-9-]+\.$2-1\$" | head -1 || true; }
# wait_until waits up to $1 seconds for a command to succeed.
wait_until() {
  local secs=$1
  shift
  for _ in $(seq $((secs * 5))); do
    "$@" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  return 1
}
serves() { [[ "$(curl -fsS --max-time 5 "${@:2}")" == *"$1"* ]]; }

step "Dokploy as its installer and its deploys leave it, on a real Swarm"
for image in "$nginx" "$postgres" "$traefik"; do
  docker pull --quiet "$image" >/dev/null || fail "can't pull $image"
done
docker network create --driver overlay --attachable dokploy-network >/dev/null || fail "can't make dokploy-network"
mkdir -p "$dynamic"
# Dokploy's file for an application: its domain to its appName's port 80.
cat >"$dynamic/shop-k2m4.yml" <<'EOF'
http:
  routers:
    shop-k2m4-router-1:
      rule: Host(`shop.example.com`)
      service: shop-k2m4-service-1
      entryPoints:
        - web
  services:
    shop-k2m4-service-1:
      loadBalancer:
        servers:
          - url: http://shop-k2m4:80
EOF
docker run --detach --name dokploy-traefik --network dokploy-network --publish "127.0.0.1:$traefik_port:80" \
  --volume "$dynamic:/etc/dokploy/traefik/dynamic:ro" "$traefik" \
  --entrypoints.web.address=:80 --providers.file.directory=/etc/dokploy/traefik/dynamic --providers.file.watch=true >/dev/null ||
  fail "can't run Dokploy's Traefik"
# An application, as Dokploy deploys one: its appName, its labels, a volume of what
# it serves, and its external port through the routing mesh.
docker volume create shop-data >/dev/null
docker run --rm --volume shop-data:/data "$nginx" sh -c 'echo shop-from-dokploy >/data/index.html' || fail "can't fill shop-data"
docker service create --quiet --detach=false --name shop-k2m4 --network dokploy-network --restart-condition any \
  --label dokploy.project=store --label dokploy.environment=production --label dokploy.application=shop \
  --mount type=volume,source=shop-data,target=/usr/share/nginx/html --publish published=8085,target=80 "$nginx" >/dev/null ||
  fail "can't create shop-k2m4"
# A stack, as Dokploy deploys a Compose app of the type stack.
stack=$(mktemp)
cat >"$stack" <<EOF
services:
  db:
    image: $postgres
    environment:
      POSTGRES_PASSWORD: not-in-any-plan
    volumes:
      - pgdata:/var/lib/postgresql/data
    networks: [dokploy-network]
    deploy:
      labels:
        dokploy.project: community
        dokploy.application: forum
  web:
    image: $nginx
    networks: [dokploy-network]
    deploy:
      labels:
        dokploy.project: community
        dokploy.application: forum
volumes:
  pgdata:
networks:
  dokploy-network:
    external: true
EOF
docker stack deploy --detach=false --compose-file "$stack" forum-q1 >/dev/null || fail "can't deploy the stack"
rm -f "$stack"
# What can't come: more than one replica, every node, a secret, an anonymous volume.
labels=(--label dokploy.project=store --label dokploy.environment=production)
docker service create --quiet --detach=false --name many-r2 --replicas 2 "${labels[@]}" --label dokploy.application=many "$nginx" >/dev/null ||
  fail "can't create many-r2"
docker service create --quiet --detach=false --name everywhere-g1 --mode global "${labels[@]}" --label dokploy.application=everywhere "$nginx" >/dev/null ||
  fail "can't create everywhere-g1"
printf 'not-in-any-plan' | docker secret create e2e-swarm-secret - >/dev/null || fail "can't make a secret"
docker service create --quiet --detach=false --name keeps-s1 --secret e2e-swarm-secret "${labels[@]}" --label dokploy.application=keeps "$nginx" >/dev/null ||
  fail "can't create keeps-s1"
docker service create --quiet --detach=false --name anon-a1 --mount type=volume,target=/data "${labels[@]}" --label dokploy.application=anon "$nginx" >/dev/null ||
  fail "can't create anon-a1"
wait_until 60 serves shop-from-dokploy -H 'Host: shop.example.com' "http://127.0.0.1:$traefik_port/" || fail "Traefik doesn't reach shop-k2m4 before the import"
wait_until 60 serves shop-from-dokploy "http://127.0.0.1:8085/" || fail "the routing mesh doesn't publish 8085 before the import"
db_task=$(running_tasks forum-q1_db)
[[ -n "$db_task" ]] || fail "the stack's PostgreSQL doesn't run"
wait_until 60 docker exec "$db_task" pg_isready -U postgres || fail "the stack's PostgreSQL isn't ready"
docker exec "$db_task" psql -U postgres -qc "create table kept (v text); insert into kept values ('row-from-dokploy')" >/dev/null ||
  fail "can't write a row in the stack's PostgreSQL"
shop_task=$(docker ps --format '{{.Names}}' --filter label=com.docker.swarm.service.name=shop-k2m4)
db_task_name=$(docker ps --format '{{.Names}}' --filter label=com.docker.swarm.service.name=forum-q1_db)

step "Dockwell runs as root, as on a server, and is claimed"
priv env DOCKWELL_TEST=1 DOCKWELL_TEST_PANELS="$root" "$bin" run --listen "$addr" --agent-listen 127.0.0.1:0 --data-dir "$data" --log-format json >>"$log" 2>&1 &
pid=$!
wait_until 30 curl -fsS "http://$addr/healthz" || fail "/healthz didn't answer within 30 s"
wait_until 30 grep -q 'connected to the control plane' "$log" || fail "the agent didn't connect within 30 s"
claim=$(dw claim) || fail "dockwell claim failed"
link=$(grep -o 'http://localhost:[0-9]*/claim#[A-Za-z0-9_-]*' <<<"$claim" | head -1) || true
[[ -n "$link" ]] || fail "dockwell claim printed no link: $claim"
(cd "$(dirname "$0")/.." && go run ./scripts/claim "$link" "$cookie" "$passkey" && go build -o "$stepup" ./scripts/stepup) || fail "can't claim the install"
server=$(dw servers list --json | jq -r '.servers[0].id') || fail "servers list failed"

person() {
  curl -sS --max-time 120 --resolve "localhost:$port:127.0.0.1" -H "Origin: http://localhost:$port" -H 'Content-Type: application/json' \
    -H "Cookie: $(cat "$cookie")" -d "$2" "http://localhost:$port/dockwell.v1.$1"
}
change_field() { dw changes get "$1" --json | jq -r ".change.$2"; }
# apply has a person apply a change, with a passkey where it asks for one, and
# waits for it to end; it prints how it ended.
apply() {
  local fingerprint answer proof state=""
  fingerprint=$(change_field "$1" plan.fingerprint) || fail "can't read the plan of $1"
  answer=$(person ChangeService/ApplyChange "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\"}")
  if grep -q needs_step_up <<<"$answer"; then
    proof=$("$stepup" "http://localhost:$port" "$cookie" "$passkey" STEP_UP_ACTION_APPLY_ROOT_CHANGE "$1") || fail "can't confirm $1 with the passkey"
    person ChangeService/ApplyChange "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\",\"proof\":\"$proof\"}" >/dev/null
  fi
  for _ in $(seq 360); do
    state=$(change_field "$1" state) || state=""
    [[ "$state" == CHANGE_STATE_APPLIED || "$state" == CHANGE_STATE_FAILED ]] && break
    sleep 0.5
  done
  echo "$state"
}

step "The import plans making the application and the stack again, and leaves what can't come, saying why"
since=$(date +%s.%N)
plan=$(person ImportService/ImportDokploy "{\"serverId\":\"$server\"}") || fail "a person can't reach the import"
[[ "$plan" != *not-in-any-plan* ]] || fail "the import shows a value of an environment or a secret: $plan"
step_of() { jq -c --arg n "$1" '.steps[] | select(.name == $n)' <<<"$plan"; }
for app in shop forum; do
  jq -e '.action == "IMPORT_ACTION_MAKE"' <<<"$(step_of $app)" >/dev/null || fail "$app isn't made again: $(step_of $app)"
done
for c in "many|2 replicas" "everywhere|every node" "keeps|Swarm secret" "anon|anonymous volume"; do
  app=${c%%|*}
  why=${c#*|}
  jq -e --arg why "$why" '.action == "IMPORT_ACTION_LEAVE" and (.reason | contains($why))' <<<"$(step_of "$app")" >/dev/null ||
    fail "$app doesn't stay behind for its $why: $(step_of "$app")"
done
change=$(jq -r '.change.id // empty' <<<"$plan")
[[ -n "$change" ]] || fail "the import planned nothing: $plan"

step "Applying it: each old task is gone before its copy starts"
state=$(apply "$change")
[[ "$state" == CHANGE_STATE_APPLIED ]] || fail "the import ended $state: $(dw changes get "$change" --json)"
until=$(date +%s.%N)
events=$(docker events --since "$since" --until "$until" --filter type=container --format '{{json .}}')
# at NAME ACTION is when Docker said a container of that name did that, in ns.
at() { jq -rs --arg n "$1" --arg a "$2" '[.[] | select(.Actor.Attributes.name == $n and .Action == $a) | .timeNano] | first // empty' <<<"$events"; }
shop_copy=$(copy_of store shop-k2m4)
db_copy=$(copy_of forum db)
[[ -n "$shop_copy" && -n "$db_copy" ]] || fail "no copy runs: shop $shop_copy, db $db_copy"
for pair in "$shop_task|$shop_copy" "$db_task_name|$db_copy"; do
  old=${pair%%|*}
  new=${pair#*|}
  died=$(at "$old" die)
  started=$(at "$new" start)
  [[ -n "$died" && -n "$started" ]] || fail "Docker didn't say $old stopped ($died) and $new started ($started)"
  ((died < started)) || fail "$new started at $started, before $old stopped at $died"
done
[[ "$(replicas shop-k2m4)" == 0 && -z "$(running_tasks shop-k2m4)" ]] || fail "shop-k2m4 still runs"
! docker logs "$db_copy" 2>&1 | grep -q 'postmaster.pid' || fail "the copy of PostgreSQL found another on its data: $(docker logs --tail 20 "$db_copy" 2>&1)"
[[ "$(docker exec "$db_copy" psql -U postgres -tAc 'select v from kept')" == row-from-dokploy ]] || fail "the copy of PostgreSQL doesn't hold the row"

step "Traefik still reaches the application by its appName, and the stack's service answers by its Swarm name"
wait_until 30 serves shop-from-dokploy -H 'Host: shop.example.com' "http://127.0.0.1:$traefik_port/" ||
  fail "Traefik doesn't reach the copy of shop-k2m4: $(curl -sS -H 'Host: shop.example.com' "http://127.0.0.1:$traefik_port/" 2>&1)"
docker run --rm --network dokploy-network "$nginx" wget -qO- http://forum-q1_web/ >/dev/null ||
  fail "forum-q1_web doesn't answer on dokploy-network"

step "Each copy restarts as its Swarm service did, and the published port is the copy's"
for c in "$shop_copy" "$db_copy"; do
  policy=$(docker inspect --format '{{.HostConfig.RestartPolicy.Name}}' "$c")
  [[ "$policy" == unless-stopped ]] || fail "$c restarts $policy, not unless-stopped as its Swarm service did"
done
[[ "$(docker service inspect --format '{{len .Endpoint.Ports}}' shop-k2m4)" == 0 ]] || fail "shop-k2m4 still publishes $(docker service inspect --format '{{json .Endpoint.Ports}}' shop-k2m4)"
wait_until 30 serves shop-from-dokploy "http://127.0.0.1:8085/" || fail "the copy doesn't answer on 8085"
docker port "$shop_copy" | grep -q ':8085$' || fail "the copy doesn't publish 8085: $(docker port "$shop_copy")"

step "Undoing it puts each Swarm service back as it was"
undo=$(person ChangeService/UndoChange "{\"id\":\"$change\"}" | jq -r '.change.id // empty')
[[ -n "$undo" ]] || fail "undoing the import planned nothing"
state=$(apply "$undo")
[[ "$state" == CHANGE_STATE_APPLIED ]] || fail "the undo ended $state: $(dw changes get "$undo" --json)"
[[ "$(replicas shop-k2m4)" == 1 ]] || fail "shop-k2m4 runs $(replicas shop-k2m4) after the undo"
[[ "$(docker service inspect --format '{{len .Endpoint.Ports}}' shop-k2m4)" == 1 ]] || fail "shop-k2m4 doesn't publish 8085 again"
[[ -z "$(copy_of store shop-k2m4)" && -z "$(copy_of forum db)" ]] || fail "a copy is left after the undo"
wait_until 60 serves shop-from-dokploy -H 'Host: shop.example.com' "http://127.0.0.1:$traefik_port/" || fail "Traefik doesn't reach shop-k2m4 after the undo"
wait_until 60 serves shop-from-dokploy "http://127.0.0.1:8085/" || fail "8085 doesn't answer after the undo"
step "all good"
