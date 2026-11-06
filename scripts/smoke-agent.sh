#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Checks the agent build end to end, beside the full build
# (docs/adr/0023-a-full-build-and-an-agent-build.md): each says which build
# it is, the agent build never starts a control plane, and says where the
# commands it lacks are, and it joins a control plane that the full build
# runs and connects to it.
#
#   scripts/smoke-agent.sh bin/dockwell bin/dockwell-agent
set -euo pipefail

usage="usage: scripts/smoke-agent.sh path/to/dockwell path/to/dockwell-agent"
full=${1:?$usage}
agent=${2:?$usage}
work=$(mktemp -d)
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

fail() {
  echo "smoke-agent: $*" >&2
  for log in "$work"/*.log; do
    [[ -s "$log" ]] || continue
    echo "--- $(basename "$log")" >&2
    cat "$log" >&2
  done
  exit 1
}

# Each build says which it is, in the shape install.sh reads. What it says is read whole
# and then matched: piped into grep -q, the program can die of SIGPIPE as grep leaves,
# which pipefail reads as a failed check, though the words were there.
said=$("$full" version --json) || fail "$full version --json failed"
grep -q '"edition": *"BUILD_EDITION_FULL"' <<<"$said" || fail "$full doesn't say it is the full build"
said=$("$agent" version --json) || fail "$agent version --json failed"
grep -q '"edition": *"BUILD_EDITION_AGENT"' <<<"$said" || fail "$agent doesn't say it is the agent build"
said=$("$agent" version) || fail "$agent version failed"
grep -q '^build  *agent$' <<<"$said" || fail "dockwell version doesn't say it is the agent build"

# Without an identity, the agent build explains, and makes nothing.
code=0
"$agent" run --data-dir "$work/agent.d" --log-format json >"$work/refused.log" 2>&1 || code=$?
((code == 1)) || fail "the agent build's run without an identity exited $code, want 1"
grep -q "hasn't joined a control plane, and this is the agent build" "$work/refused.log" || fail "run without an identity didn't say why"
[[ ! -e "$work/agent.d/control" ]] || fail "the agent build made a control plane's directory"
[[ "$("$agent" role --data-dir "$work/agent.d")" == none ]] || fail "dockwell role before joining isn't none"
for cmd in "token create" claim "servers list"; do
  code=0
  # shellcheck disable=SC2086 # split into the command's words
  "$agent" $cmd >"$work/elsewhere.log" 2>&1 || code=$?
  ((code == 2)) || fail "dockwell $cmd in the agent build exited $code, want 2"
  grep -q "the agent build of dockwell" "$work/elsewhere.log" || fail "dockwell $cmd in the agent build didn't say it isn't there"
done
rm -f "$work/refused.log" "$work/elsewhere.log"

# A control plane of the full build, and the agent build joined to it. It takes
# any free port, so this can run beside another test of the built binaries.
"$full" run --listen 127.0.0.1:0 --agent-listen 127.0.0.1:0 --data-dir "$work/control.d" --log-format json >"$work/control.log" 2>&1 &
pids+=($!)
addr=
for _ in $(seq 100); do
  addr=$(sed -n 's/.*"listener":"control plane","addr":"\([^"]*\)".*/\1/p' "$work/control.log")
  [[ -n "$addr" ]] && curl -fsS "http://$addr/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
[[ -n "$addr" ]] || fail "the full build's control plane didn't say where it listens within 10 s"
curl -fsS "http://$addr/healthz" >/dev/null 2>&1 || fail "the full build's control plane didn't answer on $addr within 10 s"
ticket=$("$full" token create --data-dir "$work/control.d" 2>>"$work/control.log" | awk '$1 == "dockwell" && $2 == "join" { print $3 }')
[[ -n "$ticket" ]] || fail "dockwell token create printed no ticket"
"$agent" join --data-dir "$work/agent.d" --name smoke-agent "$ticket" >"$work/join.log" 2>&1 || fail "the agent build didn't join"
[[ "$("$agent" role --data-dir "$work/agent.d")" == "agent https://"* ]] || fail "dockwell role after joining isn't agent"
"$agent" run --data-dir "$work/agent.d" --log-format json >"$work/agent.log" 2>&1 &
pids+=($!)
connected=0
for _ in $(seq 150); do
  connected=$(awk -f "$(dirname "$0")/connected.awk" "$work/control.log")
  ((connected >= 2)) && break
  sleep 0.1
done
((connected >= 2)) || fail "the agent build didn't connect to the control plane within 15 s"
grep -q '"msg":"server connected".*"name":"smoke-agent".*"build":"agent"' "$work/control.log" ||
  fail "the control plane didn't see the agent build in its hello"

echo "smoke-agent: the agent build says which it is, runs no control plane, says where its other commands are, and joins and connects"
