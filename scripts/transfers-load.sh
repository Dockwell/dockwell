#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Measures an agent of the agent build while files upload and download through the
# REST paths, as the panel sends them (docs/adr/0045-a-file-manager-on-a-server.md):
# four uploads at once, in 64 MiB parts, each a PUT, which is every transfer place
# a server has and one waiting, then four downloads at once. The agent holds a frame
# or two of each, never the file, so it fails past the agent's budget of 40 MB,
# which budgets.sh holds an idle agent to. Every byte is checked both ways.
#
# On a machine of 16 CPUs, an agent settled at 29 MB peaks at about 34 MB here,
# where it peaked at 40 MB while each part came packed with gzip and landed in
# buffers of its own (#883). Most of the 34 MB is the agent at rest, and 20 MB of
# that is pages of the binary.
#
#   scripts/transfers-load.sh bin/dockwell bin/dockwell-agent [MiB]
set -euo pipefail
# Anything this starts asks nothing beyond this machine (DOCKWELL_TEST, ADR 0034).
export DOCKWELL_TEST=1

usage="usage: scripts/transfers-load.sh path/to/dockwell path/to/dockwell-agent [MiB]"
full=$(realpath "${1:?$usage}")
agent=$(realpath "${2:?$usage}")
mib=${3:-512}
settle=${LOAD_SETTLE:-30}
max_mb=${LOAD_MAX_MB:-40}
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
pids=()
# shellcheck disable=SC2317,SC2329 # the EXIT trap runs it
cleanup() {
  for pid in "${pids[@]}" $(jobs -p); do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

fail() {
  echo "transfers-load: $*" >&2
  for log in "$work"/*.log; do
    [[ -s "$log" ]] || continue
    echo "--- $(basename "$log")" >&2
    tail -20 "$log" >&2
  done
  exit 1
}

# MB here is 10^6 bytes, as budgets.sh and the PRD's budgets count it; procfs
# gives KiB.
rss() { awk '/^VmRSS:/ { printf "%.1f", $2 * 1024 / 1e6 }' "/proc/$1/status"; }
# The most it has ever held, which the kernel keeps: sampling between parts misses it.
peak() { awk '/^VmHWM:/ { printf "%.1f", $2 * 1024 / 1e6 }' "/proc/$1/status"; }

# The limit the installed service sets, as budgets.sh reads it.
GOMEMLIMIT=$(sed -n 's/^MEMORY_LIMIT=//p' "$root/scripts/install.sh")
[[ -n $GOMEMLIMIT ]] || fail "can't read MEMORY_LIMIT from scripts/install.sh"
export GOMEMLIMIT

(cd "$root" && go build -o "$work/claim" ./scripts/claim)
# Each from its own copy, as on a server of its own.
cp "$full" "$work/control"
cp "$agent" "$work/agent"
python3 "$root/scripts/artifact-load.py" fixture "$work" "$mib"
go build -o "$work/fakedocker" "$root/scripts/fakedocker"
DOCKWELL_TEST_ARCHIVE_LAYER="$work/layer.tar" "$work/fakedocker" "$work/docker" >"$work/docker.log" 2>&1 &
pids+=($!)
for _ in $(seq 100); do
  [[ -S "$work/docker/docker.sock" ]] && break
  sleep 0.1
done
[[ -S "$work/docker/docker.sock" ]] || fail "the fake Engine did not start"


# Both agent editions use the same local synthetic Engine.
export DOCKWELL_LOG_FORMAT=text DOCKWELL_DATA_DIR=$work/control.d
DOCKER_HOST=unix://$work/docker/docker.sock "$work/control" run --listen 127.0.0.1:0 --agent-listen 127.0.0.1:0 --log-format json >"$work/control.log" 2>&1 &
control_pid=$!
pids+=("$control_pid")
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
link=$("$work/control" claim 2>>"$work/control.log" | grep -o 'http://localhost:[0-9]*/claim#[A-Za-z0-9_-]*' | head -1) || true
[[ -n "$link" ]] || fail "dockwell claim printed no claim link"
"$work/claim" "$link" "$work/cookie" >/dev/null || fail "can't claim the install"
cookie=$(<"$work/cookie")
origin=${link%%/claim#*}

ticket=$("$work/control" token create 2>>"$work/control.log" | awk '$1 == "dockwell" && $2 == "join" { print $3 }')
[[ -n "$ticket" ]] || fail "dockwell token create printed no ticket"
"$work/agent" join --data-dir "$work/agent.d" --name load-agent "$ticket" >"$work/join.log" 2>&1 || fail "the agent build didn't join"
DOCKER_HOST=unix://$work/docker/docker.sock "$work/agent" run --data-dir "$work/agent.d" --log-format json >"$work/agent.log" 2>&1 &
agent_pid=$!
pids+=("$agent_pid")
for _ in $(seq 150); do
  grep -q 'connected to the control plane' "$work/agent.log" && break
  sleep 0.1
done
grep -q 'connected to the control plane' "$work/agent.log" || fail "the agent build didn't connect"

# api calls the REST API as the panel does, signed in.
api() {
  local verb=$1 path=$2
  shift 2
  curl -sS -X "$verb" -H "Cookie: $cookie" -H "Origin: $origin" -H "Sec-Fetch-Site: same-origin" "$@" "$origin$path"
}
id=$(api GET /v1/servers | jq -r '.servers[] | select(.name == "load-agent") | .id')
[[ -n $id ]] || fail "the joined agent isn't among the servers"

"$full" login --local >/dev/null 2>&1 || fail "can't sign in on the control server"
"$full" services create transfer-probe --on "$id" --app-image fixture/transfer:prints --yes >"$work/probe.log" 2>&1 ||
  fail "can't run the log probe"

sleep "$settle"
settled=$(rss "$agent_pid")
echo "agent build, settled: $settled MB"

# Keep the bytes incompressible without letting random data happen to look like
# a password-hash line, which downloads refuse. The size and byte checks stay.
head -c "$((mib << 20))" /dev/urandom | LC_ALL=C tr ':' '_' >"$work/source.bin"
size=$(stat -c %s "$work/source.bin")
sum=$(sha256sum "$work/source.bin" | cut -d' ' -f1)
mkdir -p "$work/dest"

# upload sends the source to $1 in 64 MiB parts, each from where the last ended. A
# part the server had no place for goes again, as the panel's does.
upload() {
  local to=$1 began upload offset=0 part=$((64 << 20)) code
  began=$(api POST "/v1/servers/$id/uploads" -H "Content-Type: application/json" -d "{\"path\":\"$to\",\"sizeBytes\":\"$size\"}")
  upload=$(jq -r .upload.uploadId <<<"$began")
  [[ -n $upload && $upload != null ]] || fail "BeginUpload for $to: $began"
  while ((offset < size)); do
    code=$(dd if="$work/source.bin" bs=1M skip=$((offset >> 20)) count=$((part >> 20)) status=none |
      api PUT "/v1/servers/$id/uploads/$upload?offset=$offset" -H "Content-Type: application/octet-stream" --data-binary @- -o "$to.put" -w '%{http_code}')
    case $code in
    200) offset=$((offset + part < size ? offset + part : size)) ;;
    429) sleep 2 ;;
    *) fail "PUT $to at $offset: $code $(cat "$to.put")" ;;
    esac
  done
  api POST "/v1/servers/$id/uploads/$upload:finish" -H "Content-Type: application/json" -d '{}' >"$to.finish"
  jq -e .path "$to.finish" >/dev/null || fail "FinishUpload for $to: $(cat "$to.finish")"
}

# download reads $1 and prints what it holds, as a checksum.
download() {
  local code
  while :; do
    api GET "/v1/servers/$id/files:download?path=$1" -D "$2.headers" | sha256sum | cut -d' ' -f1 >"$2" ||
      fail "GET $1 was cut short"
    code=$(awk 'NR == 1 { print $2 }' "$2.headers")
    [[ $code != 429 ]] && break
    sleep 2
  done
  [[ $code == 200 ]] || fail "GET $1: $code"
}

went=$SECONDS
each=()
for i in 1 2 3 4; do
  upload "$work/dest/up-$i.bin" &
  each+=($!)
done
for job in "${each[@]}"; do wait "$job" || fail "an upload failed"; done
for i in 1 2 3 4; do
  cmp -s "$work/source.bin" "$work/dest/up-$i.bin" || fail "what landed at up-$i.bin isn't what was sent"
done
echo "four uploads of $mib MiB at once, in $((SECONDS - went)) s: agent at most $(peak "$agent_pid") MB"
rm -f "$work"/dest/up-[234].bin

went=$SECONDS
each=()
for i in 1 2 3 4; do
  download "$work/dest/up-1.bin" "$work/down-$i.sum" &
  each+=($!)
done
for job in "${each[@]}"; do wait "$job" || fail "a download failed"; done
for i in 1 2 3 4; do
  [[ $(<"$work/down-$i.sum") == "$sum" ]] || fail "what came down the ${i}th time isn't what was there"
done
most=$(peak "$agent_pid")
echo "four downloads of $mib MiB at once, in $((SECONDS - went)) s: agent at most $most MB, budget $max_mb MB"
awk -v v="$most" -v l="$max_mb" 'BEGIN { exit !(v < l) }' || fail "the agent's memory came to $most MB, past its budget of $max_mb"

# Measure four concurrent max-size (1 MiB) binary image preview reads (N1).
preview_file="$work/dest/preview-1m.png"
{ printf '\x89PNG\r\n\x1a\n'; head -c $((1048576 - 8)) /dev/urandom | LC_ALL=C tr ':' '_'; } >"$preview_file"
preview_b64_len=$(base64 -w0 "$preview_file" | wc -c)
went=$SECONDS
each=()
for i in 1 2 3 4; do
  api POST "/v1/servers/$id/files:read" -H "Content-Type: application/json" \
    -d "{\"path\":\"$preview_file\",\"allowBinary\":true}" >"$work/preview-$i.json" &
  each+=($!)
done
for job in "${each[@]}"; do wait "$job" || fail "a preview read failed"; done
for i in 1 2 3 4; do
  got_len=$(jq -r '.rawContent | length' "$work/preview-$i.json")
  [[ "$got_len" == "$preview_b64_len" ]] || fail "preview $i returned $got_len base64 bytes, want $preview_b64_len"
done
most=$(peak "$agent_pid")
echo "four 1 MiB binary previews at once, in $((SECONDS - went)) s: agent at most $most MB, budget $max_mb MB"
awk -v v="$most" -v l="$max_mb" 'BEGIN { exit !(v < l) }' || fail "the agent's memory after previews came to $most MB, past its budget of $max_mb"
rm -f "$preview_file" "$work"/preview-*.json
DOCKWELL_LOAD_COOKIE="$cookie" python3 "$root/scripts/artifact-load.py" run "$work" "$origin" "$id" "$agent_pid" "$max_mb" ||
  fail "the artifact transfer run failed"
own_id=$(api GET /v1/servers | jq -r '.servers[] | select(.controlServer) | .id')
[[ -n $own_id ]] || fail "the control server isn't among the servers"
echo "full build with embedded agent, 250 MB budget"
DOCKWELL_LOAD_COOKIE="$cookie" python3 "$root/scripts/artifact-load.py" run "$work" "$origin" "$own_id" "$control_pid" 250 ||
  fail "the embedded-agent artifact transfer run failed"
sleep 3
echo "agent after artifact cleanup: $(rss "$agent_pid") MB RSS"
echo "transfers-load: ok"
