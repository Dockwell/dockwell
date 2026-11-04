#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Measures a built dockwell binary at idle and fails when it is over the
# PRD's footprint budgets (docs/product/prd.md#non-functional-requirements):
#
#   an agent on its own                    under 40 MB RSS
#   a control plane with its own agent     under 250 MB RSS
#
# It warns when an agent passes 30 MB, well before it fails.
#
#   scripts/budgets.sh bin/dockwell [results.json]
#   BASELINE=main/dockwell scripts/budgets.sh bin/dockwell [results.json]
#
# A run starts a control plane, lets it settle and measures it. Then it
# joins a second data directory to it as an agent-only server, lets that
# settle and measures the agent. Each figure is the median of RUNS runs (5
# by default). With BASELINE, as many runs of that binary go alongside, on
# the same machine, so the two compare without the difference between
# machines or moments in it. footprint-comment.yml posts that comparison on
# pull requests. Only this build's runs can fail the check: when a run of
# the baseline fails, its logs are shown and there's no comparison.
#
# Runs start STAGGER seconds apart (3 by default), alternating between the
# two binaries, so that no two start at once and contend for the CPU.
#
# Each process runs from its own copy of the binary, as on a server of its
# own, so it shares no pages and its PSS is what it costs that server. Runs
# use throwaway data directories and loopback ports, as scripts/smoke.sh
# does, and the GOMEMLIMIT the installed service sets.
#
# A process's figures are the highest of 5 samples, 1 s apart, taken after
# SETTLE seconds (180 by default). An idle process's memory climbs for its
# first 2 to 2.5 minutes, until the collector's first forced cycle, and only
# slowly after that, so a sample at 30 s read an agent about 2 MB low
# (docs/adr/0018-keep-the-budgets-in-rss.md):
#
#   RSS        resident memory, as top shows it; the budgets are stated in it
#   PSS        resident memory, with each page shared with other processes
#              split among them
#   anonymous  memory the process allocated, which only swap can take back.
#              The rest of RSS is pages of the binary, which the kernel can
#              drop and read again.
#
# MB here is 10^6 bytes. The results go to $GITHUB_STEP_SUMMARY when it is
# set, and as JSON to the second argument.
set -euo pipefail

usage="usage: [RUNS=5] [SETTLE=180] [STAGGER=3] [BASELINE=other/dockwell] scripts/budgets.sh path/to/dockwell [results.json]"
bin=$(realpath "${1:?$usage}")
out=${2:-}
baseline=
if [[ -n ${BASELINE:-} ]]; then baseline=$(realpath "$BASELINE"); fi
baseline_name=${BASELINE_NAME:-the baseline}
settle=${SETTLE:-180}
runs=${RUNS:-5}
stagger=${STAGGER:-3}
# Run k serves its API on port + k.
port=${BUDGETS_PORT:-7698}
whole() { [[ $2 =~ ^[0-9]+$ ]] || { echo "budgets: $1 must be a whole number, not \"$2\"" >&2; exit 2; }; }
whole RUNS "$runs"
whole SETTLE "$settle"
whole STAGGER "$stagger"
((runs > 0)) || { echo "budgets: RUNS must be 1 or more, not $runs" >&2; exit 2; }

# The budgets, and where an agent gets a warning, in bytes.
agent_budget=40000000
agent_warn=30000000
control_budget=250000000

# The limit the installed service sets, read from scripts/install.sh so the
# two can't drift.
GOMEMLIMIT=$(sed -n 's/^MEMORY_LIMIT=//p' "$(dirname "$0")/install.sh")
[[ -n $GOMEMLIMIT ]] || { echo "budgets: can't read MEMORY_LIMIT from scripts/install.sh" >&2; exit 1; }
export GOMEMLIMIT

connected_awk="$(dirname "$(realpath "$0")")/connected.awk"
work=$(mktemp -d)
# shellcheck disable=SC2317,SC2329 # the EXIT trap runs it
cleanup() {
  local pid
  local -a pids=()
  # Runs still going see this within a second and end quietly.
  touch "$work/stop"
  if [[ -s "$work/pids" ]]; then mapfile -t pids <"$work/pids"; fi
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  # The runs started them, so the shell can't wait for them; give each 10 s.
  for pid in "${pids[@]}"; do
    for _ in $(seq 100); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
  done
  wait 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

# fail ends run $1 with a message and the logs of that run, unless the
# check is already over and is stopping its processes.
fail() {
  local dir="$work/$1" log
  shift
  [[ -e $work/stop ]] && exit 1
  echo "budgets: $(<"$dir/name"), run $(basename "$dir"): $*" >&2
  for log in "$dir"/*.log; do
    [[ -s "$log" ]] || continue
    echo "--- run $(basename "$dir"), $(basename "$log")" >&2
    cat "$log" >&2
  done
  exit 1
}

# pause sleeps $1 seconds in a run, a second at a time, and ends the run
# quietly once the check is over.
pause() {
  local i
  for ((i = 0; i < $1; i++)); do
    [[ -e $work/stop ]] && exit 1
    sleep 1
  done
}

# sample prints the highest RSS, PSS and anonymous memory of pid $2 over 5
# samples, in bytes, for run $1.
sample() {
  local k=$1 pid=$2 rss=0 pss=0 anon=0 r p a
  for _ in 1 2 3 4 5; do
    r=$(awk '/^VmRSS:/ { print $2 }' "/proc/$pid/status" 2>/dev/null) || fail "$k" "process $pid exited while being measured"
    a=$(awk '/^RssAnon:/ { print $2 }' "/proc/$pid/status" 2>/dev/null) || fail "$k" "process $pid exited while being measured"
    p=$(awk '/^Pss:/ { print $2 }' "/proc/$pid/smaps_rollup" 2>/dev/null) || fail "$k" "process $pid exited while being measured"
    [[ -n $r && -n $a && -n $p ]] || fail "$k" "process $pid has no VmRSS, RssAnon or Pss"
    ((r * 1024 > rss)) && rss=$((r * 1024))
    ((p * 1024 > pss)) && pss=$((p * 1024))
    ((a * 1024 > anon)) && anon=$((a * 1024))
    sleep 1
  done
  echo "$rss $pss $anon"
}

# run measures a control plane and an agent of the binary $2, called $3, as
# run $1, and writes their figures to $work/$1/figures.
run() {
  local k=$1 src=$2 dir="$work/$1" api="127.0.0.1:$((port + $1))" control agent ticket connected figures
  mkdir -p "$dir"
  echo "$3" >"$dir/name"
  cp "$src" "$dir/control"
  cp "$src" "$dir/agent"

  # The control plane, with its own agent, in one process.
  "$dir/control" run --listen "$api" --agent-listen 127.0.0.1:0 --data-dir "$dir/control.d" --log-format json \
    >"$dir/control.log" 2>&1 &
  control=$!
  echo "$control" >>"$work/pids"
  for _ in $(seq 300); do
    curl -fsS "http://$api/healthz" >/dev/null 2>&1 && break
    sleep 0.1
  done
  curl -fsS "http://$api/healthz" >/dev/null 2>&1 || fail "$k" "the control plane didn't answer /healthz on $api within 30 s"
  pause "$settle"
  kill -0 "$control" 2>/dev/null || fail "$k" "the control plane exited before it was measured"
  figures=$(sample "$k" "$control")

  # An agent-only server, joined to it with a ticket.
  ticket=$("$dir/control" token create --data-dir "$dir/control.d" 2>>"$dir/control.log" |
    awk '$1 == "dockwell" && $2 == "join" { print $3 }')
  [[ -n "$ticket" ]] || fail "$k" "dockwell token create printed no ticket"
  "$dir/agent" join --data-dir "$dir/agent.d" --name "budgets-agent-$k" "$ticket" >"$dir/join.log" 2>&1 ||
    fail "$k" "dockwell join failed"
  "$dir/agent" run --data-dir "$dir/agent.d" --log-format json >"$dir/agent.log" 2>&1 &
  agent=$!
  echo "$agent" >>"$work/pids"
  connected=0
  for _ in $(seq 300); do
    connected=$(awk -f "$connected_awk" "$dir/control.log")
    ((connected >= 2)) && break
    sleep 0.1
  done
  ((connected >= 2)) || fail "$k" "the joined agent didn't connect within 30 s"
  pause "$settle"
  kill -0 "$agent" 2>/dev/null || fail "$k" "the agent exited before it was measured"
  figures+=" $(sample "$k" "$agent")"
  echo "$figures" >"$dir/figures"
}

# median prints the median of its arguments.
median() {
  local -a s
  mapfile -t s < <(printf '%s\n' "$@" | sort -n)
  local n=${#s[@]}
  if ((n % 2)); then echo "${s[n / 2]}"; else echo $(((s[n / 2 - 1] + s[n / 2]) / 2)); fi
}

mb() { awk -v b="$1" 'BEGIN { printf "%.1f MB", b / 1e6 }'; }
budget() { awk -v b="$1" 'BEGIN { printf "%d MB", b / 1e6 }'; }
delta() { awk -v d="$1" 'BEGIN { printf "%+.1f MB", d / 1e6 }'; }
status() { if (($1 < $2)); then echo "Within budget"; else echo "Over budget"; fi; }

# Field i (0 to 5) of the figures of runs first to first + runs - 1:
# control RSS, PSS and anonymous, then the agent's.
column() {
  local i=$1 first=$2 k
  local -a f
  for ((k = first; k < first + runs; k++)); do
    read -ra f <"$work/$k/figures"
    echo "${f[i]}"
  done
}

if [[ -n $baseline ]]; then
  echo "budgets: measuring $runs runs each of $bin and $baseline, $stagger s apart, after $settle s each"
else
  echo "budgets: measuring $runs runs of $bin, $stagger s apart, after $settle s each"
fi
# Runs 0 to runs - 1 are this build's, and the next as many the baseline's.
this_jobs=()
base_jobs=()
for ((k = 0; k < runs; k++)); do
  ((k == 0)) || sleep "$stagger"
  run "$k" "$bin" "this build" &
  this_jobs+=($!)
  if [[ -n $baseline ]]; then
    sleep "$stagger"
    run "$((runs + k))" "$baseline" "$baseline_name" &
    base_jobs+=($!)
  fi
done
failed=0
for job in "${this_jobs[@]}"; do wait "$job" || failed=1; done
((failed == 0)) || exit 1
# A baseline that fails takes the comparison with it, not the check.
baseline_failed=
for job in "${base_jobs[@]}"; do wait "$job" || baseline_failed=1; done
if [[ -n $baseline_failed ]]; then
  note="Runs of $baseline_name failed, so there's no comparison with it. Their logs are in this check's output. Every run of this build finished, and only this build's figures can fail the check."
  if [[ ${GITHUB_ACTIONS:-} == true ]]; then
    echo "::warning title=No comparison::$note"
  else
    echo "budgets: $note" >&2
  fi
  baseline=
fi

# medians fills the associative array named $2 with the medians of one
# binary's runs, starting at run $1: control_rss and so on, and for RSS each
# run's figure (control_rss_runs) and their range (control_rss_range).
# shellcheck disable=SC2034 # into names the caller's array
medians() {
  local first=$1 i name
  local -n into=$2
  local -a names=(control_rss control_pss control_anon agent_rss agent_pss agent_anon) col
  for i in "${!names[@]}"; do
    name=${names[i]}
    mapfile -t col < <(column "$i" "$first")
    into["$name"]=$(median "${col[@]}")
    if [[ $name == *_rss ]]; then
      into["${name}_runs"]=$(IFS=,; echo "${col[*]}")
      into["${name}_range"]=$(printf '%s\n' "${col[@]}" | sort -n |
        awk 'NR == 1 { lo = $1 } { hi = $1 } END { printf "%.1f to %.1f MB", lo / 1e6, hi / 1e6 }')
    fi
  done
}
declare -A this=() base=()
medians 0 this
this[version]=$("$bin" version | awk '$1 == "dockwell" { print $2 }')
this[commit]=$("$bin" version | awk '$1 == "commit" { print $2 }')
if [[ -n $baseline ]]; then
  medians "$runs" base
  base[version]=$("$baseline" version 2>/dev/null | awk '$1 == "dockwell" { print $2 }' || true)
  base[commit]=$("$baseline" version 2>/dev/null | awk '$1 == "commit" { print $2 }' || true)
fi
agent_rss=${this[agent_rss]}
control_rss=${this[control_rss]}

table=$(
  cat <<EOF
| Idle, after $settle s | RSS | PSS | Anonymous | Budget | Result |
| --- | ---: | ---: | ---: | ---: | --- |
| An agent on its own | $(mb "$agent_rss") | $(mb "${this[agent_pss]}") | $(mb "${this[agent_anon]}") | $(budget "$agent_budget") | $(status "$agent_rss" "$agent_budget") |
| A control plane with its own agent | $(mb "$control_rss") | $(mb "${this[control_pss]}") | $(mb "${this[control_anon]}") | $(budget "$control_budget") | $(status "$control_rss" "$control_budget") |
EOF
)
echo "$table"
warning=
if ((agent_rss >= agent_warn && agent_rss < agent_budget)); then
  warning="An agent on its own idles at $(mb "$agent_rss") RSS, over the $(budget "$agent_warn") warning line. This check fails at $(budget "$agent_budget"). Compare with main's figures to find what grew it, and trim it while the change is small."
  if [[ ${GITHUB_ACTIONS:-} == true ]]; then
    echo "::warning title=Agent memory::$warning"
  else
    echo "budgets: $warning" >&2
  fi
fi
comparison=
if [[ -n $baseline ]]; then
  comparison=$(
    cat <<EOF
| Idle, after $settle s, RSS | This build | $baseline_name | Change | Runs of this build | Runs of $baseline_name |
| --- | ---: | ---: | ---: | ---: | ---: |
| An agent on its own | $(mb "$agent_rss") | $(mb "${base[agent_rss]}") | $(delta $((agent_rss - base[agent_rss]))) | ${this[agent_rss_range]} | ${base[agent_rss_range]} |
| A control plane with its own agent | $(mb "$control_rss") | $(mb "${base[control_rss]}") | $(delta $((control_rss - base[control_rss]))) | ${this[control_rss_range]} | ${base[control_rss_range]} |
EOF
  )
  echo
  echo "$comparison"
fi
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "### Memory at idle"
    echo
    echo "$table"
    echo
    if [[ -n $warning ]]; then
      echo "**$warning**"
      echo
    fi
    echo "dockwell ${this[version]}${this[commit]:+ at ${this[commit]}}, with GOMEMLIMIT=$GOMEMLIMIT." \
      "Each figure is the median of $runs runs, and a run's is the highest of 5 samples, 1 s apart." \
      "Anonymous memory is what dockwell allocated. The rest of RSS is pages of the binary, which the kernel can drop and read again."
    if [[ -n $comparison ]]; then
      echo
      echo "#### Against $baseline_name"
      echo
      echo "$comparison"
      echo
      echo "Both were measured on this runner, $runs runs each, alternating and started $stagger s apart." \
        "$baseline_name is dockwell ${base[version]:-of an unknown version}${base[commit]:+ at ${base[commit]}}."
    elif [[ -n $baseline_failed ]]; then
      echo
      echo "**$note**"
    fi
  } >>"$GITHUB_STEP_SUMMARY"
fi

# figures prints the figures in the associative array named $1 as JSON
# members, indented by $2.
figures() {
  local -n of=$1
  local in=$2 v
  printf '%s"version": "%s",\n%s"commit": "%s",\n' "$in" "${of[version]}" "$in" "${of[commit]}"
  for v in agent_rss agent_pss agent_anon control_rss control_pss control_anon; do
    printf '%s"%s_bytes": %s,\n' "$in" "$v" "${of[$v]}"
  done
  printf '%s"agent_rss_runs_bytes": [%s],\n%s"control_rss_runs_bytes": [%s]' \
    "$in" "${of[agent_rss_runs]}" "$in" "${of[control_rss_runs]}"
}
if [[ -n "$out" ]]; then
  {
    echo "{"
    figures this "  "
    echo ","
    if [[ -n $baseline ]]; then
      echo '  "baseline": {'
      figures base "    "
      echo
      echo "  },"
    fi
    printf '  "settle_seconds": %s,\n  "stagger_seconds": %s,\n  "runs": %s,\n' "$settle" "$stagger" "$runs"
    printf '  "agent_budget_bytes": %s,\n  "agent_warn_bytes": %s,\n  "control_budget_bytes": %s\n' \
      "$agent_budget" "$agent_warn" "$control_budget"
    echo "}"
  } >"$out"
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
