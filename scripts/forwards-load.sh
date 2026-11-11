#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Measures an agent of the agent build while four apps opened through the panel
# carry as many connections as it forwards at once, 16 (ADR 0061): for each, two
# uploads to an app that reads them slowly, which is where an agent holds what the
# browser sent, and two downloads, each through its own connection. The agent holds
# a window and a message or two of each forward, never the body, so it fails past
# the agent's budget of 40 MB, which budgets.sh holds an idle agent to. It runs on
# the fake Docker Engine, with a small HTTP server where each app's port is
# published.
#
#   scripts/forwards-load.sh bin/dockwell bin/dockwell-agent [MiB]
set -euo pipefail
# Anything this starts asks nothing beyond this machine (DOCKWELL_TEST, ADR 0034).
export DOCKWELL_TEST=1

usage="usage: scripts/forwards-load.sh path/to/dockwell path/to/dockwell-agent [MiB]"
full=$(realpath "${1:?$usage}")
agent=$(realpath "${2:?$usage}")
mib=${3:-64}
settle=${LOAD_SETTLE:-30}
max_mb=${LOAD_MAX_MB:-40}
apps=4
ups=${LOAD_UPS:-2}     # uploads to each app at once
downs=${LOAD_DOWNS:-2} # and downloads
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
  echo "forwards-load: $*" >&2
  for log in "$work"/*.log; do
    [[ -s "$log" ]] || continue
    echo "--- $(basename "$log")" >&2
    tail -20 "$log" >&2
  done
  exit 1
}

rss() { awk '/^VmRSS:/ { printf "%.1f", $2 / 1024 }' "/proc/$1/status"; }
# The most it has ever held, which the kernel keeps: sampling misses it.
peak() { awk '/^VmHWM:/ { printf "%.1f", $2 / 1024 }' "/proc/$1/status"; }

# The limit the installed service sets, as budgets.sh reads it.
GOMEMLIMIT=$(sed -n 's/^MEMORY_LIMIT=//p' "$root/scripts/install.sh")
[[ -n $GOMEMLIMIT ]] || fail "can't read MEMORY_LIMIT from scripts/install.sh"
export GOMEMLIMIT

(cd "$root" && go build -o "$work/fakedocker" ./scripts/fakedocker && go build -o "$work/claim" ./scripts/claim &&
  go build -o "$work/stepup" ./scripts/stepup) || fail "can't build the helpers"
"$work/fakedocker" "$work/docker" >"$work/fakedocker.log" 2>&1 &
pids+=($!)
socket=
for _ in $(seq 100); do
  socket=$(head -1 "$work/fakedocker.log" || true)
  [[ -n "$socket" && -S "$socket" ]] && break
  sleep 0.1
done
[[ -n "$socket" && -S "$socket" ]] || fail "the fake engine didn't start"

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
"$work/claim" "$link" "$work/cookie" "$work/passkey" >/dev/null || fail "can't claim the install"
"$full" login --local >/dev/null 2>&1 || fail "can't sign in on the control server"
origin=${link%%/claim#*}
port=${origin##*:}
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
server=$("$full" servers list --json | jq -r '.servers[] | select(.name == "load-agent") | .id')
[[ -n $server ]] || fail "the joined agent isn't among the servers"

# api calls the panel's API as the panel does, signed in.
api() {
  curl -sS --max-time 30 -H "Cookie: $(<"$work/cookie")" -H "Origin: $origin" -H 'Content-Type: application/json' "$@"
}

# Each app: a service publishing its port 80 on loopback, then, where it's published,
# a server that streams what's asked of it and reads what's sent slowly.
cat >"$work/app.py" <<'EOF'
import http.server, sys, time
class App(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_GET(self):
        size = int(sys.argv[2]) << 20
        self.send_response(200)
        self.send_header("Content-Length", str(size))
        self.end_headers()
        chunk = b"x" * 65536
        for _ in range(size // len(chunk)):
            self.wfile.write(chunk)
    def do_POST(self):
        left = int(self.headers["Content-Length"])
        while left > 0:
            got = self.rfile.read(min(16384, left))
            if not got:
                break
            left -= len(got)
            time.sleep(0.005)
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")
    def log_message(self, *args):
        pass
http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), App).serve_forever()
EOF
opened=()
for i in $(seq "$apps"); do
  at=$((19400 + i))
  "$full" services create "app-$i" --app-image load/app:1 --on "$server" -y \
    --app-ports "{\"hostIp\":\"127.0.0.1\",\"hostPort\":$at,\"containerPort\":80}" >"$work/create-$i.log" 2>&1 ||
    fail "can't run app-$i: $(tail -3 "$work/create-$i.log")"
done
for i in $(seq "$apps"); do
  at=$((19400 + i))
  python3 -I "$work/app.py" "$at" "$mib" >"$work/app-$i.log" 2>&1 &
  pids+=($!)
  id=$("$full" services list --json | jq -r --arg n "app-$i" '.services[] | select(.resource.name == $n) | .id')
  [[ -n $id ]] || fail "app-$i isn't among the services"
  proof=$("$work/stepup" "$origin" "$work/cookie" "$work/passkey" STEP_UP_ACTION_OPEN_SERVICE "$id") || fail "can't confirm opening app-$i"
  res=$(api -X POST -d "{\"proof\":\"$proof\"}" "$origin/v1/services/$id:open")
  url=$(jq -r .url <<<"$res")
  [[ $url == http://*.localhost:* ]] || fail "OpenService for app-$i: $res"
  host=${url#http://}
  host=${host%%/*}
  label=${host%%:*}
  cookie=$(curl -sS --resolve "$label:$port:127.0.0.1" -D - -o /dev/null "$url" | sed -n 's/^[Ss]et-[Cc]ookie: dockwell_open=\([^;]*\).*/\1/p' | tr -d '\r')
  [[ -n $cookie ]] || fail "opening app-$i set no cookie"
  opened+=("$label $cookie")
done
for _ in $(seq 50); do
  curl -fsS -o /dev/null "http://127.0.0.1:19401/" 2>/dev/null && break
  sleep 0.1
done

sleep "$settle"
echo "agent build, settled with $apps apps open: $(rss "$agent_pid") MB"

head -c "$((mib << 20))" /dev/urandom >"$work/body.bin"
went=$SECONDS
jobs_=()
for entry in "${opened[@]}"; do
  read -r label cookie <<<"$entry"
  for n in $(seq "$ups"); do
    curl -sS --resolve "$label:$port:127.0.0.1" -H "Cookie: dockwell_open=$cookie" --data-binary "@$work/body.bin" \
      -o "$work/up-$label-$n" "http://$label:$port/up" &
    jobs_+=($!)
  done
  for n in $(seq "$downs"); do
    curl -sS --resolve "$label:$port:127.0.0.1" -H "Cookie: dockwell_open=$cookie" -o "$work/down-$label-$n" "http://$label:$port/big" &
    jobs_+=($!)
  done
done
for job in "${jobs_[@]}"; do wait "$job" || fail "a request through an opened app failed"; done
for entry in "${opened[@]}"; do
  read -r label _ <<<"$entry"
  for n in $(seq "$ups"); do
    [[ $(<"$work/up-$label-$n") == ok ]] || fail "an upload to $label answered $(head -c 200 "$work/up-$label-$n")"
  done
  for n in $(seq "$downs"); do
    size=$(stat -c %s "$work/down-$label-$n")
    ((size == mib << 20)) || fail "a download from $label came to $size bytes"
  done
done
most=$(peak "$agent_pid")
echo "$((apps * (ups + downs))) forwards at once, $((apps * ups)) uploads to apps that read slowly and $((apps * downs)) downloads of $mib MiB, in $((SECONDS - went)) s: agent at most $most MB, budget $max_mb MB"
awk -v v="$most" -v l="$max_mb" 'BEGIN { exit !(v < l) }' || fail "the agent's memory came to $most MB, past its budget of $max_mb"
echo "forwards-load: ok"
