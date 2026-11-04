#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Measures an agent while a container writes as fast as it can be read, on the
# fake Docker Engine, so it needs no Docker: what the agent build holds in memory
# with a reader following, then with a reader that has stopped reading, and that
# its stats keep coming all the while. Each log stream's lines are limited and
# dropped on the agent, so this should read about the same whatever the flood's
# length. See docs/adr/0025-logs-per-deploy.md.
#
#   scripts/logs-load.sh bin/dockwell bin/dockwell-agent [seconds]
set -euo pipefail

usage="usage: scripts/logs-load.sh path/to/dockwell path/to/dockwell-agent [seconds]"
full=$(realpath "${1:?$usage}")
agent=$(realpath "${2:?$usage}")
seconds=${3:-30}
root=$(cd "$(dirname "$0")/.." && pwd)
addr=127.0.0.1:${LOAD_PORT:-7695}
load=127.0.0.1:${LOAD_CONTROL_PORT:-7696}
work=$(mktemp -d)
pids=()
cleanup() {
  for pid in "${pids[@]}"; do
    kill -CONT "$pid" 2>/dev/null || true
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

fail() {
  echo "logs-load: $*" >&2
  for log in "$work"/*.log; do
    [[ -s "$log" ]] || continue
    echo "--- $(basename "$log")" >&2
    tail -20 "$log" >&2
  done
  exit 1
}

rss() { awk '/^VmRSS:/ { printf "%.1f", $2 / 1024 }' "/proc/$1/status"; }
cpu_ticks() { awk '{ print $14 + $15 }' "/proc/$1/stat"; }

# sample runs for n seconds, and prints the median and largest RSS of pid, and the
# share of one CPU it took.
sample() {
  local pid=$1 n=$2 file="$work/samples" start end
  : >"$file"
  start=$(cpu_ticks "$pid")
  for _ in $(seq "$n"); do
    rss "$pid" >>"$file"
    echo >>"$file"
    sleep 1
  done
  end=$(cpu_ticks "$pid")
  sort -n "$file" | awk -v ticks="$((end - start))" -v n="$n" -v hz="$(getconf CLK_TCK)" '
    { v[NR] = $1 } END { printf "median %.1f MB, largest %.1f MB, %.0f%% of a CPU", v[int((NR + 1) / 2)], v[NR], 100 * ticks / hz / n }'
}

(cd "$root" && go build -o "$work/logload" ./scripts/logload && go build -o "$work/claim" ./scripts/claim)
"$work/logload" "$work/docker" "$load" >"$work/logload.log" 2>&1 &
pids+=($!)
for _ in $(seq 100); do
  [[ -S "$work/docker/docker.sock" ]] && break
  sleep 0.1
done
[[ -S "$work/docker/docker.sock" ]] || fail "the fake engine didn't start"

# The control plane's own agent has no Docker, so the service goes on the agent build.
export DOCKWELL_LOG_FORMAT=text DOCKWELL_DATA_DIR=$work/control
DOCKER_HOST=unix://$work/none.sock "$full" run --listen "$addr" --agent-listen 127.0.0.1:0 --log-format json >"$work/control.log" 2>&1 &
control=$!
pids+=("$control")
for _ in $(seq 100); do
  curl -fsS "http://$addr/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "http://$addr/healthz" >/dev/null 2>&1 || fail "the control plane didn't start"
link=$(grep -o 'http://localhost:[0-9]*/claim#[A-Za-z0-9_-]*' "$work/control.log" | head -1) || true
[[ -n "$link" ]] || fail "dockwell run printed no claim link"
"$work/claim" "$link" >/dev/null || fail "can't claim the install"
"$full" login --local >/dev/null 2>&1 || fail "can't sign in on the control server"
ticket=$("$full" token create 2>>"$work/control.log" | awk '$1 == "dockwell" && $2 == "join" { print $3 }')
[[ -n "$ticket" ]] || fail "dockwell token create printed no ticket"
"$agent" join --data-dir "$work/agent.d" --name load-agent "$ticket" >"$work/join.log" 2>&1 || fail "the agent build didn't join"
DOCKER_HOST=unix://$work/docker/docker.sock "$agent" run --data-dir "$work/agent.d" --log-format json >"$work/agent.log" 2>&1 &
agent_pid=$!
pids+=("$agent_pid")
for _ in $(seq 150); do
  grep -q 'connected to the control plane' "$work/agent.log" && break
  sleep 0.1
done
grep -q 'connected to the control plane' "$work/agent.log" || fail "the agent build didn't connect"
sleep 2
server=$("$full" servers list --json | jq -r '.servers[] | select(.name == "load-agent") | .id')
"$full" services create web --on "$server" --app-image demo/app:1 --yes >"$work/deploy.log" 2>&1 || fail "can't run a service"

last_seen() { "$full" servers list --json | jq -r '.servers[] | select(.name == "load-agent") | .lastSeenTime'; }

echo "agent build, before any reader: $(rss "$agent_pid") MB"
"$full" logs follow web --tail -1 --json >/dev/null 2>"$work/follow.log" &
follower=$!
pids+=("$follower")
sleep 1
curl -fsS -X POST "http://$load/flood?size=200" >/dev/null
sleep 2
echo "a reader following a flood, for $seconds s: agent $(sample "$agent_pid" "$seconds")"
echo "  control plane: $(rss "$control") MB"
wrote=$(curl -fsS "http://$load/written")
echo "  the container wrote $wrote lines"

seen=$(last_seen)
kill -STOP "$follower"
echo "a reader that stopped reading, for $seconds s: agent $(sample "$agent_pid" "$seconds")"
echo "  control plane: $(rss "$control") MB"
now=$(last_seen)
[[ "$now" != "$seen" ]] || fail "the agent's stats stopped coming while its reader was stuck"
if grep -q 'closing a silent stream' "$work/control.log"; then
  fail "the control plane closed the agent's stream as silent during a flood"
fi
echo "  the agent's stats kept coming: last seen moved from $seen to $now"
kill -CONT "$follower"

curl -fsS -X POST "http://$load/flood?size=0" >/dev/null
echo "after the flood, reader gone:"
kill "$follower" 2>/dev/null || true
sleep 5
echo "  agent $(rss "$agent_pid") MB, control plane $(rss "$control") MB; readers left on Docker: $(curl -fsS "http://$load/followers")"
