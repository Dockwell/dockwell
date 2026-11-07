#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Measures an agent under what log readers can ask of it, on the fake Docker
# Engine, so it needs no Docker: a container that writes as fast as it can be read,
# with a reader following, one following with a filter, many following, one that
# has stopped reading; then searches of a million short lines at once. Its memory
# and CPU have limits below, and it fails past them. Each log stream's lines are
# limited and dropped on the agent, so this should read about the same whatever
# the flood's length. See docs/adr/0025-logs-per-deploy.md.
#
#   scripts/logs-load.sh bin/dockwell bin/dockwell-agent [seconds]
set -euo pipefail
# Anything this starts asks nothing beyond this machine (DOCKWELL_TEST, ADR 0034).
export DOCKWELL_TEST=1

usage="usage: scripts/logs-load.sh path/to/dockwell path/to/dockwell-agent [seconds]"
full=$(realpath "${1:?$usage}")
agent=$(realpath "${2:?$usage}")
seconds=${3:-30}
root=$(cd "$(dirname "$0")/.." && pwd)
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

# What one stage may take of the agent, past which this fails: MB of memory it can
# hold, and how much of a CPU. The figures in the ADR are what these are set from,
# with room for a slower machine than the one that measured them.
max_flood_mb=${LOAD_MAX_FLOOD_MB:-40}
max_search_mb=${LOAD_MAX_SEARCH_MB:-80}
max_follow_cpu=${LOAD_MAX_FOLLOW_CPU:-100}
max_many_cpu=${LOAD_MAX_MANY_CPU:-150}

# under fails unless a figure is within its limit.
under() {
  local what=$1 value=$2 limit=$3
  awk -v v="$value" -v l="$limit" 'BEGIN { exit !(v <= l) }' || fail "$what came to $value, past its limit of $limit"
}

# sample runs for n seconds, and sets the median and largest RSS of pid, and the
# share of one CPU it took.
median=0 largest=0 cpu=0
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
  read -r median largest cpu < <(sort -n "$file" | awk -v ticks="$((end - start))" -v n="$n" -v hz="$(getconf CLK_TCK)" '
    { v[NR] = $1 } END { printf "%.1f %.1f %.0f\n", v[int((NR + 1) / 2)], v[NR], 100 * ticks / hz / n }')
}

said() { echo "median $median MB, largest $largest MB, $cpu% of a CPU"; }

(cd "$root" && go build -o "$work/logload" ./scripts/logload && go build -o "$work/claim" ./scripts/claim)
# Ports that are free, as on one someone else holds, this would measure their server,
# and read its own log for what that server did.
"$work/logload" "$work/docker" 127.0.0.1:0 >"$work/logload.log" 2>&1 &
pids+=($!)
load=
for _ in $(seq 100); do
  load=$(sed -n 's/^logload: listening on //p' "$work/logload.log")
  [[ -S "$work/docker/docker.sock" && -n "$load" ]] && break
  sleep 0.1
done
[[ -S "$work/docker/docker.sock" && -n "$load" ]] || fail "the fake engine didn't start"

# The control plane's own agent has no Docker, so the service goes on the agent build.
export DOCKWELL_LOG_FORMAT=text DOCKWELL_DATA_DIR=$work/control
DOCKER_HOST=unix://$work/none.sock "$full" run --listen 127.0.0.1:0 --agent-listen 127.0.0.1:0 --log-format json >"$work/control.log" 2>&1 &
control=$!
pids+=("$control")
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
# A limit a real server's process wouldn't reach, which holding a search to its bound
# has to stay well within: without one, Go's collector lets a heap grow to twice
# what it holds.
GOMEMLIMIT=200MiB DOCKER_HOST=unix://$work/docker/docker.sock "$agent" run --data-dir "$work/agent.d" --log-format json >"$work/agent.log" 2>&1 &
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

flood() { curl -fsS -X POST "http://$load/flood?size=$1" >/dev/null; }

echo "agent build, before any reader: $(rss "$agent_pid") MB"
"$full" logs follow web --tail -1 --json >/dev/null 2>"$work/follow.log" &
follower=$!
pids+=("$follower")
sleep 1
flood 200
sleep 2
wrote_before=$(curl -fsS "http://$load/written")
sample "$agent_pid" "$seconds"
echo "a reader following a flood, for $seconds s: agent $(said)"
under "the agent's memory with a reader following a flood" "$largest" "$max_flood_mb"
under "the agent's CPU with a reader following a flood" "$cpu" "$max_follow_cpu"
echo "  control plane: $(rss "$control") MB"
wrote=$(curl -fsS "http://$load/written")
echo "  the container wrote $wrote lines, $(((wrote - wrote_before) / seconds)) a second"

seen=$(last_seen)
kill -STOP "$follower"
sample "$agent_pid" "$seconds"
echo "a reader that stopped reading, for $seconds s: agent $(said)"
under "the agent's memory with a reader stuck" "$largest" "$max_flood_mb"
under "the agent's CPU with a reader stuck" "$cpu" "$max_follow_cpu"
echo "  control plane: $(rss "$control") MB"
now=$(last_seen)
[[ "$now" != "$seen" ]] || fail "the agent's stats stopped coming while its reader was stuck"
if grep -q 'closing a silent stream' "$work/control.log"; then
  fail "the control plane closed the agent's stream as silent during a flood"
fi
echo "  the agent's stats kept coming: last seen moved from $seen to $now"
kill -CONT "$follower"
kill "$follower" 2>/dev/null || true
wait "$follower" 2>/dev/null || true

"$full" logs follow web --tail -1 --query never-printed --json >/dev/null 2>"$work/filtered.log" &
follower=$!
pids+=("$follower")
sleep 2
sample "$agent_pid" "$seconds"
echo "a reader following with a filter nothing matches: agent $(said)"
under "the agent's memory with a filtered follow" "$largest" "$max_flood_mb"
under "the agent's CPU with a filtered follow" "$cpu" "$max_follow_cpu"
kill "$follower" 2>/dev/null || true
wait "$follower" 2>/dev/null || true

many=()
for _ in $(seq 20); do
  "$full" logs follow web --tail -1 --json >/dev/null 2>"$work/many.log" &
  many+=($!)
done
pids+=("${many[@]}")
sleep 3
sample "$agent_pid" "$seconds"
followers=$(curl -fsS "http://$load/followers")
echo "20 followers of a flood, from one reader: agent $(said); $followers on Docker; control plane $(rss "$control") MB"
# One reader holds at most 8 of a server's 16 places, and a test holds 16 to the
# same pace (TestFollowsAreHeldToThePace).
[[ "$followers" == 8 ]] || fail "$followers followers reached Docker, want 8, the most one reader can have"
under "the agent's memory with 20 followers" "$largest" "$max_flood_mb"
under "the agent's CPU with 20 followers" "$cpu" "$max_many_cpu"
for pid in "${many[@]}"; do kill "$pid" 2>/dev/null || true; done
wait "${many[@]}" 2>/dev/null || true

flood 0
echo "after the flood, readers gone:"
sleep 5
echo "  agent $(rss "$agent_pid") MB, control plane $(rss "$control") MB; readers left on Docker: $(curl -fsS "http://$load/followers")"

awk 'BEGIN { for (i = 0; i < 1000000; i++) print "x" }' >"$work/lines"
curl -fsS -X POST --data-binary @"$work/lines" "http://$load/print" >/dev/null || fail "can't write the lines to search"
watcher() {
  while kill -0 "$agent_pid" 2>/dev/null; do
    rss "$agent_pid" >>"$work/search-rss"
    echo >>"$work/search-rss"
    sleep 0.1
  done
}
watcher &
watching=$!
pids+=("$watching")
search() { "$full" logs follow web --once --tail 100000 --query x --json >/dev/null; }
: >"$work/search-rss"
search 2>"$work/search.log" || fail "a search failed: $(cat "$work/search.log")"
sleep 1
largest=$(sort -n "$work/search-rss" | tail -1)
echo "a search of 1,000,000 lines of one letter: agent's largest $largest MB, with GOMEMLIMIT=200MiB"
under "the agent's memory with one search" "$largest" "$max_search_mb"
: >"$work/search-rss"
searches=()
for _ in $(seq 8); do
  search 2>>"$work/search.log" &
  searches+=($!)
done
wait "${searches[@]}" || true
sleep 1
largest=$(sort -n "$work/search-rss" | tail -1)
echo "8 at once, the most one reader can: agent's largest $largest MB; control plane $(rss "$control") MB"
under "the agent's memory with 8 searches" "$largest" "$max_search_mb"
kill "$watching" 2>/dev/null || true
kill -0 "$agent_pid" 2>/dev/null || fail "the agent stopped during the searches"
echo "logs-load: ok"
