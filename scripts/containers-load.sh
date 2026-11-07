#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Measures an agent on a server of hundreds of containers it didn't make, on the
# fake Docker Engine, so it needs no Docker: settled, and then with the panel's list
# of them read again and again. The agent decodes Docker's list a container at a time
# and sends a bounded message, so this should read about the same at any number of
# containers. It fails past the agent's budget of 40 MB, which budgets.sh holds an
# idle agent with no containers to. See docs/adr/0031-containers-dockwell-didnt-make.md.
#
#   scripts/containers-load.sh bin/dockwell bin/dockwell-agent [containers]
set -euo pipefail
# Anything this starts asks nothing beyond this machine (DOCKWELL_TEST, ADR 0034).
export DOCKWELL_TEST=1

usage="usage: scripts/containers-load.sh path/to/dockwell path/to/dockwell-agent [containers]"
full=$(realpath "${1:?$usage}")
agent=$(realpath "${2:?$usage}")
containers=${3:-500}
seconds=${LOAD_SECONDS:-60}
settle=${LOAD_SETTLE:-30}
max_mb=${LOAD_MAX_MB:-40}
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
pids=()
# shellcheck disable=SC2317,SC2329 # the EXIT trap runs it
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

fail() {
  echo "containers-load: $*" >&2
  for log in "$work"/*.log; do
    [[ -s "$log" ]] || continue
    echo "--- $(basename "$log")" >&2
    tail -20 "$log" >&2
  done
  exit 1
}

rss() { awk '/^VmRSS:/ { printf "%.1f", $2 / 1024 }' "/proc/$1/status"; }
# The most it has ever held, which the kernel keeps: sampling between reads misses it.
peak() { awk '/^VmHWM:/ { printf "%.1f", $2 / 1024 }' "/proc/$1/status"; }

# The limit the installed service sets, as budgets.sh reads it.
GOMEMLIMIT=$(sed -n 's/^MEMORY_LIMIT=//p' "$root/scripts/install.sh")
[[ -n $GOMEMLIMIT ]] || fail "can't read MEMORY_LIMIT from scripts/install.sh"
export GOMEMLIMIT

(cd "$root" && go build -o "$work/fakedocker" ./scripts/fakedocker && go build -o "$work/claim" ./scripts/claim)
"$work/fakedocker" "$work/docker" >"$work/fakedocker.log" 2>&1 &
pids+=($!)
socket=
for _ in $(seq 100); do
  socket=$(head -1 "$work/fakedocker.log" || true)
  [[ -n "$socket" && -S "$socket" ]] && break
  sleep 0.1
done
[[ -n "$socket" && -S "$socket" ]] || fail "the fake engine didn't start"

# What a real server's containers look like to a list: most are in Compose projects,
# with Compose's labels, a volume or two and a published port. Nothing is on a port
# another holds, so each gets its own.
engine() { curl --fail --silent --show-error --unix-socket "$socket" "$@"; }
engine -X POST 'http://docker/v1.47/images/create?fromImage=load/app&tag=1' >/dev/null || fail "the fake engine won't pull"
for ((i = 0; i < containers; i++)); do
  project=stack-$((i / 4))
  body=$(jq -nc --arg name "load-$i" --arg project "$project" --arg service "app-$((i % 4))" --argjson port "$((20000 + i))" '{
    Image: "load/app:1",
    Env: ["PASSWORD=not-for-a-list", "DATABASE_URL=postgres://user:not-for-a-list@db/app"],
    Labels: {
      "com.docker.compose.project": $project, "com.docker.compose.service": $service,
      "com.docker.compose.container-number": "1", "com.docker.compose.oneoff": "False",
      "com.docker.compose.config-hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
      "com.docker.compose.image": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
      "com.docker.compose.project.config_files": "/opt/stacks/app/docker-compose.yml",
      "com.docker.compose.project.working_dir": "/opt/stacks/app",
      "com.docker.compose.version": "2.38.2", "com.docker.compose.depends_on": ""
    },
    HostConfig: {
      PortBindings: {"80/tcp": [{HostPort: ($port | tostring)}]},
      Binds: [($name + "-data:/data"), "/srv/shared:/shared:ro"]
    }
  }')
  id=$(engine -X POST -H 'Content-Type: application/json' -d "$body" "http://docker/v1.47/containers/create?name=load-$i" | jq -r .Id) ||
    fail "the fake engine won't make load-$i"
  engine -X POST "http://docker/v1.47/containers/$id/start" >/dev/null || fail "the fake engine won't start load-$i"
done

# The control plane's own agent has no Docker, so what's measured is the agent build's.
export DOCKWELL_LOG_FORMAT=text DOCKWELL_DATA_DIR=$work/control
DOCKER_HOST=unix://$work/none.sock "$full" run --listen 127.0.0.1:0 --agent-listen 127.0.0.1:0 --log-format json >"$work/control.log" 2>&1 &
pids+=($!)
addr=
for _ in $(seq 100); do
  addr=$(sed -n 's/.*"listener":"control plane","addr":"\([^"]*\)".*/\1/p' "$work/control.log")
  [[ -n "$addr" ]] && curl -fsS "http://$addr/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
if [[ -z "$addr" ]] || ! curl -fsS "http://$addr/healthz" >/dev/null 2>&1; then
  fail "the control plane didn't start"
fi
# dockwell run prints a claim link only to a terminal.
link=$("$full" claim 2>>"$work/control.log" | grep -o 'http://localhost:[0-9]*/claim#[A-Za-z0-9_-]*' | head -1) || true
[[ -n "$link" ]] || fail "dockwell claim printed no claim link"
"$work/claim" "$link" >/dev/null || fail "can't claim the install"
"$full" login --local >/dev/null 2>&1 || fail "can't sign in on the control server"
ticket=$("$full" token create 2>>"$work/control.log" | awk '$1 == "dockwell" && $2 == "join" { print $3 }')
[[ -n "$ticket" ]] || fail "dockwell token create printed no ticket"
"$agent" join --data-dir "$work/agent.d" --name load-agent "$ticket" >"$work/join.log" 2>&1 || fail "the agent build didn't join"
DOCKER_HOST=unix://$socket "$agent" run --data-dir "$work/agent.d" --log-format json >"$work/agent.log" 2>&1 &
agent_pid=$!
pids+=("$agent_pid")
for _ in $(seq 150); do
  grep -q 'connected to the control plane' "$work/agent.log" && break
  sleep 0.1
done
grep -q 'connected to the control plane' "$work/agent.log" || fail "the agent build didn't connect"

sleep "$settle"
settled=$(rss "$agent_pid")
echo "agent build, settled with $containers containers: $settled MB"

listed=$("$full" containers list --server load-agent --json) || fail "containers list failed"
count=$(jq '.containers | length' <<<"$listed") || fail "containers list printed what isn't JSON"
((count == containers)) || fail "the list has $count of $containers containers"
[[ "$listed" != *not-for-a-list* ]] || fail "the list shows a value of a container's environment"
bytes=${#listed}
echo "a list of $count containers comes to $((bytes / 1024)) KiB as JSON"

reads=0
went=$(date +%s%N)
end=$((SECONDS + seconds))
while ((SECONDS < end)); do
  "$full" containers list --server load-agent --json >/dev/null || fail "containers list failed"
  reads=$((reads + 1))
done
each=$((($(date +%s%N) - went) / reads / 1000000))
most=$(peak "$agent_pid")
echo "read $reads times, $each ms each with the CLI's own start: agent at most $most MB, budget $max_mb MB"
awk -v v="$most" -v l="$max_mb" 'BEGIN { exit !(v < l) }' || fail "the agent's memory came to $most MB, past its budget of $max_mb"
echo "containers-load: ok"
