#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Measures an agent of the agent build while files upload and download through the
# REST paths, as the panel sends them (docs/adr/0045-a-file-manager-on-a-server.md):
# four uploads at once, in 64 MiB parts, each a PUT, which is every transfer place
# a server has and one waiting, then four downloads at once, and sealed SFTP
# copies alongside panel transfers. The agent holds a frame
# or two of each, never the file, so it fails past the agent's budget of 40 MB,
# which budgets.sh holds an idle agent to. Every byte is checked both ways.
#
# On a machine of 16 CPUs, an agent settled at 32 MB peaks at about 38 MB here,
# where it peaked at 40 MB while each part came packed with gzip and landed in
# buffers of its own (#883), and again on 16 CPUs before ADR 0109. Most of the 38 MB
# is the agent at rest, and 21 MB of that is pages of the binary.
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
TMPDIR="$work" DOCKWELL_TEST_VOLUME_MOUNTPOINTS=1 DOCKWELL_TEST_ARCHIVE_LAYER="$work/layer.tar" "$work/fakedocker" "$work/docker" >"$work/docker.log" 2>&1 &
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

# shellcheck source=scripts/load-files.sh
source "$root/scripts/load-files.sh"
id=$(api GET /v1/servers | jq -r '.servers[] | select(.name == "load-agent") | .id')
[[ -n $id ]] || fail "the joined agent isn't among the servers"

"$full" login --local >/dev/null 2>&1 || fail "can't sign in on the control server"
"$full" services create transfer-probe --on "$id" --app-image fixture/transfer:prints --yes >"$work/probe.log" 2>&1 ||
  fail "can't run the log probe"

sleep "$settle"
cpu() {
  # /proc/stat CPU times are in system clock ticks.
  awk -v hz="$(getconf CLK_TCK)" '{printf "%.2f", ($14 + $15) / hz}' "/proc/$agent_pid/stat"
}
cpu_before=$(cpu)
settled=$(rss "$agent_pid")
echo "agent build, settled: $settled MB"

# Keep the bytes incompressible without letting random data happen to look like
# a password-hash line, which downloads refuse. The size and byte checks stay.
head -c "$((mib << 20))" /dev/urandom | LC_ALL=C tr ':' '_' >"$work/source.bin"
source_file="$work/source.bin"
size=$(stat -c %s "$source_file")
sum=$(sha256sum "$work/source.bin" | cut -d' ' -f1)
mkdir -p "$work/dest"

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

echo "panel upload and download agent CPU time: $(awk -v v="$(cpu)" -v b="$cpu_before" 'BEGIN { printf "%.2f", v-b }') s"

# Provision a real SFTP destination and a volume containing the same
# incompressible bytes the panel transfers use. Both are synthetic fixtures.
go build -o "$work/sftpserve" "$root/scripts/sftpserve"
ssh-keygen -q -t ed25519 -N "" -f "$work/sftp-key"
mkdir "$work/offsite"
DOCKWELL_TEST_SFTP_DISK=1 "$work/sftpserve" 127.0.0.1:0 "$work/sftp-key.pub" >"$work/sftp.log" 2>&1 &
pids+=($!)
for _ in $(seq 100); do
  [[ -s "$work/sftp.log" ]] && break
  sleep 0.1
done
read -r sftp_addr sftp_pin <"$work/sftp.log" || fail "the SFTP fixture did not start"
plan_payload=$(jq -n --rawfile key "$work/sftp-key" --argjson port "${sftp_addr##*:}" \
  --arg pin "$sftp_pin" --arg dest "$work/offsite" --arg server "$id" '{
    summary:"Set up the transfer fixtures",
    edits:[
      {resource:{kind:"volume",name:"transfer-data"},servers:[$server],spec:{volume:{}}},
      {resource:{kind:"backup.destination",name:"transfer-sftp"},spec:{backupDestination:{
        passphrase:"example load test recovery passphrase",
        sftp:{host:"127.0.0.1",port:$port,user:"backups",path:$dest,hostKeyFingerprint:$pin,
              hostKeyAlgorithm:"ssh-ed25519",privateKey:$key}
      }}}
    ]
  }')
change=$(api POST /v1/changes -H "Content-Type: application/json" -d "$plan_payload")
volume=$(jq -er '.change.plan.resources[] | select(.resource.kind == "volume") | .id' <<<"$change") ||
  fail "the SFTP fixture plan did not include a volume: $change"
change_id=$(jq -er .change.id <<<"$change")
fingerprint=$(jq -er .change.plan.fingerprint <<<"$change")
api POST "/v1/changes/$change_id:apply" -H "Content-Type: application/json" \
  -d "$(jq -n --arg fingerprint "$fingerprint" '{fingerprint:$fingerprint}')" >"$work/sftp-plan.log"
for _ in $(seq 150); do
  state=$(api GET "/v1/changes/$change_id" | jq -r .change.state)
  [[ $state == CHANGE_STATE_APPLIED ]] && break
  [[ $state != CHANGE_STATE_FAILED ]] || fail "the SFTP fixture plan failed"
  sleep 0.1
done
[[ $state == CHANGE_STATE_APPLIED ]] || fail "the SFTP fixture plan did not apply"
volume_path=$(api GET "/v1/volumes/$volume" | jq -er --arg server "$id" '.volume.servers[] | select(.serverId == $server) | .path')
[[ -d $volume_path ]] || fail "the SFTP fixture volume has no local directory"
cp --reflink=auto "$work/source.bin" "$volume_path/payload"

wait_backup() {
  local filter=$1 result
  for _ in $(seq 1200); do
    result=$(api GET "/v1/backups/$backup_id")
    if jq -e "$filter" >/dev/null <<<"$result"; then return; fi
    if jq -e '.backup.state == "BACKUP_STATE_FAILED" or .backup.copy == "BACKUP_COPY_FAILED" or (.backup.fetchError // "") != ""' >/dev/null <<<"$result"; then
      fail "the SFTP copy failed: $result"
    fi
    sleep 0.25
  done
  fail "the SFTP copy did not finish: $result"
}

cpu_before=$(cpu)
went=$SECONDS
started=$(api POST /v1/backups -H "Content-Type: application/json" \
  -d "$(jq -n --arg volume "$volume" --arg server "$id" '{volumeId:$volume,server:$server,destination:"transfer-sftp"}')")
backup_id=$(jq -er .backup.id <<<"$started") || fail "the SFTP backup did not start: $started"
# Begin the two panel transfers as the SFTP archive is being staged.
for ((attempt = 0; attempt < 1200; attempt++)); do
  [[ -n $(find "$work/offsite" -ignore_readdir_race -name '.dockwell-partial-*' -size +0c -print -quit) ]] && break
  [[ -n $(find "$work/offsite" -ignore_readdir_race -name '*.sealed' -print -quit) ]] &&
    fail "the SFTP copy finished before the panel transfers started"
  sleep 0.05
done
((attempt < 1200)) || fail "the SFTP copy did not start staging"
upload "$work/dest/up-sftp-1.bin" &
sftp_panel_1=$!
upload "$work/dest/up-sftp-2.bin" &
sftp_panel_2=$!
wait_backup '.backup.copy == "BACKUP_COPY_STORED"'
wait "$sftp_panel_1" || fail "the first panel upload beside SFTP failed"
wait "$sftp_panel_2" || fail "the second panel upload beside SFTP failed"
cmp -s "$work/source.bin" "$work/dest/up-sftp-1.bin" || fail "the first panel upload changed beside SFTP"
cmp -s "$work/source.bin" "$work/dest/up-sftp-2.bin" || fail "the second panel upload changed beside SFTP"
echo "SFTP send and two panel uploads, $((SECONDS - went)) s, CPU $(awk -v v="$(cpu)" -v b="$cpu_before" 'BEGIN { printf "%.2f", v-b }') s, agent at most $(peak "$agent_pid") MB"

api DELETE "/v1/backups/$backup_id?serverOnly=true" >"$work/sftp-remove.log"
wait_backup '.backup.onServer != true and .backup.copy == "BACKUP_COPY_STORED"'
cpu_before=$(cpu)
went=$SECONDS
api POST "/v1/backups/$backup_id:fetch" -H "Content-Type: application/json" \
  -d "$(jq -n --arg server "$id" '{server:$server}')" >"$work/sftp-fetch.log"
# Let fetch hold its transfer places before the two panel requests arrive.
for ((attempt = 0; attempt < 1200; attempt++)); do
  [[ -n $(find "$work/agent.d/agent/backups" -ignore_readdir_race -name "$backup_id.partial" -size +0c -print -quit) ]] && break
  [[ -n $(find "$work/agent.d/agent/backups" -ignore_readdir_race -name "$backup_id.json" -print -quit) ]] &&
    fail "the SFTP fetch finished before the panel transfers started"
  sleep 0.05
done
((attempt < 1200)) || fail "the SFTP fetch did not start staging"
download "$work/dest/up-1.bin" "$work/down-sftp-1.sum" &
sftp_panel_1=$!
download "$work/dest/up-1.bin" "$work/down-sftp-2.sum" &
sftp_panel_2=$!
wait_backup '.backup.onServer == true'
wait "$sftp_panel_1" || fail "the first panel download beside SFTP failed"
wait "$sftp_panel_2" || fail "the second panel download beside SFTP failed"
[[ $(<"$work/down-sftp-1.sum") == "$sum" && $(<"$work/down-sftp-2.sum") == "$sum" ]] ||
  fail "a panel download changed beside SFTP"
most=$(peak "$agent_pid")
echo "SFTP fetch with two panel downloads (panel requests retry during fetch), $((SECONDS - went)) s, CPU $(awk -v v="$(cpu)" -v b="$cpu_before" 'BEGIN { printf "%.2f", v-b }') s, agent at most $most MB, budget $max_mb MB"
awk -v v="$most" -v l="$max_mb" 'BEGIN { exit !(v < l) }' || fail "the agent's memory after SFTP came to $most MB, past its budget of $max_mb"
rm -f "$work/dest"/up-sftp-{1,2}.bin

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
