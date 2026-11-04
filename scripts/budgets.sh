#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Measures a built dockwell binary at idle and fails when it is over the
# PRD's footprint budgets (docs/product/prd.md#non-functional-requirements):
#
#   an agent on its own                    under 40 MB RSS
#   a control plane with its own agent     under 250 MB RSS
#
# It starts a control plane, lets it settle and measures it. Then it joins a
# second data directory to it as an agent-only server, lets that settle and
# measures the agent. Both use throwaway data directories and loopback ports,
# as scripts/smoke.sh does, and the GOMEMLIMIT the installed service sets.
#
#   scripts/budgets.sh bin/dockwell [results.json]
#
# Each figure is the highest of 5 samples of VmRSS, 1 s apart, taken after
# SETTLE seconds (30 by default). MB here is 10^6 bytes. The results go to
# $GITHUB_STEP_SUMMARY when it is set, and as JSON to the second argument.
set -euo pipefail

bin=$(realpath "${1:?usage: scripts/budgets.sh path/to/dockwell [results.json]}")
out=${2:-}
settle=${SETTLE:-30}
api="127.0.0.1:${BUDGETS_PORT:-7698}"
base="http://$api"

# The budgets, in bytes.
agent_budget=40000000
control_budget=250000000

# The limit the installed service sets, read from scripts/install.sh so the
# two can't drift.
GOMEMLIMIT=$(sed -n 's/^MEMORY_LIMIT=//p' "$(dirname "$0")/install.sh")
[[ -n $GOMEMLIMIT ]] || { echo "budgets: can't read MEMORY_LIMIT from scripts/install.sh" >&2; exit 1; }
export GOMEMLIMIT

work=$(mktemp -d)
pids=()
# shellcheck disable=SC2317,SC2329 # the EXIT trap runs it
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  rm -rf "$work"
}
trap cleanup EXIT

fail() {
  echo "budgets: $*" >&2
  for log in "$work"/*.log; do
    [[ -s "$log" ]] || continue
    echo "--- $(basename "$log")" >&2
    cat "$log" >&2
  done
  exit 1
}

# rss prints the highest VmRSS of pid over 5 samples, in bytes.
rss() {
  local pid=$1 max=0 kb
  for _ in 1 2 3 4 5; do
    kb=$(awk '/^VmRSS:/ {print $2}' "/proc/$pid/status" 2>/dev/null) || fail "process $pid exited while being measured"
    [[ -n "$kb" ]] || fail "process $pid has no VmRSS"
    ((kb * 1024 > max)) && max=$((kb * 1024))
    sleep 1
  done
  echo "$max"
}

mb() { awk -v b="$1" 'BEGIN { printf "%.1f MB", b / 1e6 }'; }
budget() { awk -v b="$1" 'BEGIN { printf "%d MB", b / 1e6 }'; }

alive() { kill -0 "$1" 2>/dev/null || fail "$2 exited before it was measured"; }

# The control plane, with its own agent, in one process.
"$bin" run --listen "$api" --agent-listen 127.0.0.1:0 --data-dir "$work/control" --log-format json \
  >"$work/control.log" 2>&1 &
control=$!
pids+=("$control")
for _ in $(seq 100); do
  curl -fsS "$base/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "$base/healthz" >/dev/null 2>&1 || fail "the control plane didn't answer /healthz within 10 s"

echo "budgets: measuring the control plane and its agent after $settle s"
sleep "$settle"
alive "$control" "the control plane"
control_rss=$(rss "$control")

# An agent-only server, joined to it with a ticket.
ticket=$("$bin" token create --data-dir "$work/control" 2>>"$work/control.log" | awk '$1 == "dockwell" && $2 == "join" { print $3 }')
[[ -n "$ticket" ]] || fail "dockwell token create printed no ticket"
"$bin" join --data-dir "$work/agent" --name budgets-agent "$ticket" >"$work/join.log" 2>&1 ||
  fail "dockwell join failed"
"$bin" run --data-dir "$work/agent" --log-format json >"$work/agent.log" 2>&1 &
agent=$!
pids+=("$agent")

# The API needs a session, so count the servers the control plane says
# connected, in its log.
connected=0
for _ in $(seq 100); do
  # grep finds nothing until both connect; that mustn't end the script.
  connected=$({ grep '"msg":"server connected"' "$work/control.log" | grep -o '"server":"srv_[^"]*"' || true; } | sort -u | wc -l)
  ((connected >= 2)) && break
  sleep 0.1
done
((connected >= 2)) || fail "the joined agent didn't connect within 10 s"

echo "budgets: measuring the agent after $settle s"
sleep "$settle"
alive "$agent" "the agent"
agent_rss=$(rss "$agent")

version=$("$bin" version | awk '$1 == "dockwell" { print $2 }')
commit=$("$bin" version | awk '$1 == "commit" { print $2 }')

status() { if (($1 < $2)); then echo "Within budget"; else echo "Over budget"; fi; }
table=$(
  cat <<EOF
| Idle, after $settle s | RSS | Budget | Result |
| --- | ---: | ---: | --- |
| An agent on its own | $(mb "$agent_rss") | $(budget "$agent_budget") | $(status "$agent_rss" "$agent_budget") |
| A control plane with its own agent | $(mb "$control_rss") | $(budget "$control_budget") | $(status "$control_rss" "$control_budget") |
EOF
)
echo "$table"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "### Memory at idle"
    echo
    echo "$table"
    echo
    echo "dockwell $version${commit:+ at $commit}, with GOMEMLIMIT=$GOMEMLIMIT. Each figure is the highest of 5 samples, 1 s apart."
  } >>"$GITHUB_STEP_SUMMARY"
fi
if [[ -n "$out" ]]; then
  cat >"$out" <<EOF
{
  "version": "$version",
  "commit": "$commit",
  "settle_seconds": $settle,
  "agent_rss_bytes": $agent_rss,
  "agent_budget_bytes": $agent_budget,
  "control_rss_bytes": $control_rss,
  "control_budget_bytes": $control_budget
}
EOF
fi

over=0
if ((agent_rss >= agent_budget)); then
  echo "budgets: an agent on its own idles at $(mb "$agent_rss") RSS, over its $(budget "$agent_budget") budget." \
    "Compare with main's figures to find the change that grew it, then trim it or argue for a new budget in the PRD." >&2
  over=1
fi
if ((control_rss >= control_budget)); then
  echo "budgets: a control plane with its own agent idles at $(mb "$control_rss") RSS, over its $(budget "$control_budget") budget." \
    "Compare with main's figures to find the change that grew it, then trim it or argue for a new budget in the PRD." >&2
  over=1
fi
exit "$over"
