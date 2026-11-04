#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Runs a built dockwell binary against this machine's Docker Engine, end to
# end through the CLI: a service from an image runs, changes and is undone;
# the environment exports as a Compose file that docker compose reads and
# lists; a Compose file imports; and everything goes again. It uses a
# throwaway data directory and removes what it made, so it never touches a
# real install. It needs Docker with the compose plugin, curl and a free
# port (E2E_WEB_PORT, 18080 by default).
#
#   scripts/docker-e2e.sh bin/dockwell
set -euo pipefail

bin=$(realpath "${1:?usage: scripts/docker-e2e.sh path/to/dockwell}")
addr=127.0.0.1:${E2E_PORT:-7697}
web_port=${E2E_WEB_PORT:-18080}
data=$(mktemp -d)
log=$(mktemp)
project=""

"$bin" run --listen "$addr" --agent-listen 127.0.0.1:0 --data-dir "$data" --log-format json >"$log" 2>&1 &
pid=$!
cleanup() {
  if [[ -n "$project" ]]; then
    docker compose -p "$project" down --volumes --remove-orphans --timeout 1 >/dev/null 2>&1 || true
  fi
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -rf "$log" "$data"
}
trap cleanup EXIT

fail() {
  echo "docker-e2e: $*" >&2
  echo "--- dockwell log" >&2
  cat "$log" >&2
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

ports="{\"hostPort\":$web_port,\"containerPort\":80,\"hostIp\":\"127.0.0.1\"}"

step "A service from an image runs"
dw services create web --app-image nginx:1.27-alpine --app-ports "$ports" --app-env GREETING=hello --yes ||
  fail "services create failed"
container=$(docker ps --filter label=com.docker.compose.service=web --format '{{.Names}}')
[[ "$container" == env_*-web-1 ]] || fail "Docker runs $container, want env_…-web-1"
project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$container")
curl -fsS "http://127.0.0.1:$web_port/" | grep -q nginx || fail "web doesn't serve on port $web_port"
env_of() { docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container"; }
env_of | grep -qx GREETING=hello || fail "web's environment is $(env_of)"

step "It changes, and the change undoes"
dw services update web --app-image nginx:1.27-alpine --app-ports "$ports" --app-env GREETING=hi --yes ||
  fail "services update failed"
env_of | grep -qx GREETING=hi || fail "after the update, web's environment is $(env_of)"
change=$(dw changes list --json | grep -o '"id":"chg_[^"]*"' | head -1 | cut -d'"' -f4)
[[ -n "$change" ]] || fail "changes list has no change"
dw changes undo "$change" --yes || fail "changes undo $change failed"
env_of | grep -qx GREETING=hello || fail "after the undo, web's environment is $(env_of)"
drift=$(dw drift list --json)
[[ "$drift" != *'"drift"'* ]] || fail "drift after the undo: $drift"

step "The environment exports as Compose, and docker compose lists it"
dw compose export >"$data/compose.yaml" || fail "compose export failed"
grep -q "^name: $project" "$data/compose.yaml" || fail "the export isn't project $project:
$(cat "$data/compose.yaml")"
if grep -q hello "$data/compose.yaml"; then fail "the export holds an environment variable's value"; fi
GREETING=hello docker compose -f "$data/compose.yaml" config --quiet || fail "docker compose doesn't take the export"
listed=$(GREETING=hello docker compose -f "$data/compose.yaml" ps --format '{{.Service}}')
[[ "$listed" == web ]] || fail "docker compose -f <export> ps lists '$listed', want web"
[[ "$(docker compose -p "$project" ps --format '{{.Service}}')" == web ]] || fail "docker compose -p $project ps doesn't list web"

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
dw compose import --compose "$(cat "$data/stack.yaml")" --yes || fail "compose import failed"
services=$(docker compose -p "$project" ps --format '{{.Service}}' | sort | tr '\n' ' ')
[[ "$services" == "cache web " ]] || fail "after the import, docker compose lists $services"
docker volume inspect "${project}_cache-data" >/dev/null || fail "the import made no volume ${project}_cache-data"

step "Services go"
dw services delete cache --yes || fail "services delete cache failed"
dw services delete web --yes || fail "services delete web failed"
[[ -z "$(docker compose -p "$project" ps -q)" ]] || fail "containers are left after deleting the services"

# Deleting the volume destroys data, so only a person may apply it; the
# CLI says so and leaves it planned, with exit status 4.
set +e
dw volumes delete cache-data --yes >/dev/null 2>&1
status=$?
set -e
[[ "$status" == 4 ]] || fail "volumes delete exited $status, want 4: a plan that destroys data waits for a person"

echo "docker-e2e: ok"
