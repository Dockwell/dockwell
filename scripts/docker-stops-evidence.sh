#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Print what Docker leaves in a container's state, and what its events said,
# when a person stops a container against when the daemon dies under it.
#
# ADR 0054 has the agent put back what should run and nobody stopped, so it has
# to know the two apart from what Docker says, not from memory of how Docker
# behaves. This prints the evidence into the job's log. It checks nothing and
# fails only if it can't run at all. It kills and restarts the Docker daemon, so
# it runs only on a disposable host: CHAOS_HOST_FAULTS=1, with sudo -n.
#
#   CHAOS_HOST_FAULTS=1 scripts/docker-stops-evidence.sh
set -uo pipefail

if [[ ${CHAOS_HOST_FAULTS:-} != 1 ]]; then
  echo "evidence: this kills the Docker daemon. Set CHAOS_HOST_FAULTS=1 on a disposable host to run it" >&2
  exit 2
fi
if ! sudo -n true 2>/dev/null; then
  echo "evidence: it needs root through sudo -n, to kill the daemon and read its files" >&2
  exit 2
fi

image=${EVIDENCE_IMAGE:-nginx:1.27-alpine}
label=dockwell.evidence=1
work=$(mktemp -d)
# A Compose project of this run's own, so its clean-up can't take down a real one.
evidence_project=dockwell-evidence-$$
compose="$work/compose.yaml"
events_pid=

say() { printf '\nevidence: === %s\n' "$*"; }

cleanup() {
  [[ -n $events_pid ]] && kill "$events_pid" 2>/dev/null
  docker compose -p "$evidence_project" -f "$compose" down --timeout 1 >/dev/null 2>&1
  # shellcheck disable=SC2046 # one ID a word
  docker rm -f $(docker ps -aq --filter "label=$label") >/dev/null 2>&1
  rm -rf "$work"
}
trap cleanup EXIT

# states prints one line a container: its restart policy, Docker's restart
# count, the whole of State, and the mark Docker keeps on disk for a stop it
# counts as a person's, which its API doesn't show.
states() {
  local id name
  for id in $(docker ps -aq --no-trunc --filter "label=$label" | sort); do
    name=$(docker inspect --format '{{.Name}}' "$id")
    printf 'evidence: %-28s policy=%-15s restarts=%s manually_stopped=%s\n' "$name" \
      "$(docker inspect --format '{{.HostConfig.RestartPolicy.Name}}' "$id")" \
      "$(docker inspect --format '{{.RestartCount}}' "$id")" \
      "$(sudo -n jq -c '.HasBeenManuallyStopped' "/var/lib/docker/containers/$id/config.v2.json" 2>/dev/null || echo unreadable)"
    printf 'evidence:   state=%s\n' "$(docker inspect --format '{{json .State}}' "$id")"
  done
}

# events prints what the daemon said of these containers between two times.
events() {
  docker events --since "$1" --until "$2" --filter "label=$label" \
    --format '{{.Time}} {{.Type}} {{.Action}} name={{.Actor.Attributes.name}} exit={{.Actor.Attributes.exitCode}} signal={{.Actor.Attributes.signal}}' 2>&1 |
    sed 's/^/evidence:   event /'
}

wait_docker() {
  for _ in $(seq 1 90); do
    docker info >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "evidence: Docker didn't answer within 90 s" >&2
  return 1
}

run() { # name policy [command...]
  local name=$1 policy=$2
  shift 2
  docker run -d --name "ev-$name" --label "$label" --restart "$policy" "$image" "$@" >/dev/null
}

say "the host"
docker version --format 'evidence: client {{.Client.Version}}, server {{.Server.Version}}'
docker info --format 'evidence: live-restore={{.LiveRestoreEnabled}} driver={{.Driver}} cgroup={{.CgroupDriver}}/{{.CgroupVersion}} runtime={{.DefaultRuntime}}'
echo "evidence: daemon.json: $(sudo -n cat /etc/docker/daemon.json 2>/dev/null | tr -d '\n' || true)"
systemctl show docker.service -p Restart -p RestartUSec -p KillMode -p Delegate | sed 's/^/evidence: docker.service /'
systemctl show containerd.service -p ActiveState -p KillMode 2>/dev/null | sed 's/^/evidence: containerd.service /'
docker pull -q "$image" >/dev/null

# daemon prints what could tell one run of the daemon from the next: its process
# and when that started, and what its API says of itself.
daemon() {
  local pid
  pid=$(sudo -n cat /var/run/docker.pid 2>/dev/null || true)
  echo "evidence: dockerd pid=$pid started=$(ps -o lstart= -p "${pid:-0}" 2>/dev/null || true)"
  docker info --format 'evidence: info ID={{.ID}} SystemTime={{.SystemTime}} NEventsListener={{.NEventsListener}} Containers={{.Containers}} Running={{.ContainersRunning}} Stopped={{.ContainersStopped}}'
}
daemon

say "the containers: each restart policy, left running or stopped each way a person can"
cat >"$compose" <<EOF
services:
  compose-stopped:
    image: $image
    restart: unless-stopped
    labels: ["$label"]
  compose-left:
    image: $image
    restart: unless-stopped
    labels: ["$label"]
EOF
t0=$(date +%s)
for policy in unless-stopped always on-failure no; do
  run "$policy-left" "$policy"
  run "$policy-stopped" "$policy"
  run "$policy-killed" "$policy"
done
# One that ends on its own, cleanly and not, under each policy that restarts.
run unless-stopped-exit0 unless-stopped sh -c 'sleep 3'
run on-failure-exit0 on-failure sh -c 'sleep 3'
run no-exit1 no sh -c 'sleep 3; exit 1'
docker compose -p "$evidence_project" -f "$compose" up -d >/dev/null 2>&1
sleep 5

# The events as a client that stays connected hears them, to compare with what
# the daemon keeps to replay.
docker events --filter "label=$label" \
  --format '{{.Time}} {{.Type}} {{.Action}} name={{.Actor.Attributes.name}} exit={{.Actor.Attributes.exitCode}} signal={{.Actor.Attributes.signal}}' \
  >"$work/live-events.log" 2>&1 &
events_pid=$!
sleep 1

say "a person stops some: docker stop, docker kill, docker compose stop"
t1=$(date +%s)
for policy in unless-stopped always on-failure no; do
  docker stop -t 2 "ev-$policy-stopped" >/dev/null
  docker kill "ev-$policy-killed" >/dev/null
done
docker compose -p "$evidence_project" -f "$compose" stop --timeout 2 compose-stopped >/dev/null 2>&1
sleep 4
t2=$(date +%s)

say "states after the person's stops, the daemon never having gone"
states
say "events of the person's stops, replayed from the daemon ($t1 to $t2)"
events "$t1" "$t2"

say "a start Docker couldn't make, and then a person's stop: does State.Error outlast the stop?"
# The agent takes State.Error for "Docker tried to start this and couldn't". If a
# stop left it set, a container a person stopped would read as one to start.
port=$((20000 + RANDOM % 20000))
docker run -d --name ev-port-holder --label "$label" --restart no -p "127.0.0.1:$port:80" "$image" >/dev/null
# Never started: made, and its first start refused.
docker run -d --name ev-failed-first-start --label "$label" --restart always -p "127.0.0.1:$port:80" "$image" 2>&1 | tail -1 | sed 's/^/evidence:   docker run said: /'
# Ran once, was stopped, and its next start refused.
port2=$((port + 1))
docker run -d --name ev-failed-restart --label "$label" --restart always -p "127.0.0.1:$port2:80" "$image" >/dev/null
docker stop -t 2 ev-failed-restart >/dev/null
docker run -d --name ev-port-holder2 --label "$label" --restart no -p "127.0.0.1:$port2:80" "$image" >/dev/null
docker start ev-failed-restart 2>&1 | tail -1 | sed 's/^/evidence:   docker start said: /'
sleep 2
say "states with the failed starts as Docker left them"
states
tf=$(date +%s)
docker stop -t 2 ev-failed-first-start 2>&1 | sed 's/^/evidence:   docker stop said: /'
docker stop -t 2 ev-failed-restart 2>&1 | sed 's/^/evidence:   docker stop said: /'
sleep 2
say "states after a person's docker stop on each failed start"
states
say "events of those stops ($tf on)"
events "$tf" "$(date +%s)"
# Free the ports, so what the daemon does with these as it returns isn't the port's doing.
docker rm -f ev-port-holder ev-port-holder2 >/dev/null

say "the same two, started once their ports are free, and then stopped by a person: is the old error still in the state?"
# If Docker kept the error of a start it couldn't make after the container had
# started and run, a container a person then stopped would read as one Docker
# couldn't start, and the agent would start it.
docker start ev-failed-first-start ev-failed-restart 2>&1 | sed 's/^/evidence:   docker start said: /'
sleep 2
say "states once they run"
states
docker stop -t 2 ev-failed-first-start ev-failed-restart 2>&1 | sed 's/^/evidence:   docker stop said: /'
sleep 2
say "states after a person's docker stop on what had failed, then run"
states

say "a forced removal: what docker ps -a and the list API say of a container between docker rm -f's kill and its going"
# The agent takes "removing" and "dead" for no news, so a look in that moment
# isn't read as a stop. That Docker lists "removing" then, and not "exited", was
# read from its source, not seen. Five rounds, polling as fast as curl goes.
sock=/var/run/docker.sock
for round in 1 2 3 4 5; do
  docker run -d --name "ev-forced-$round" --label "$label" --restart unless-stopped "$image" >/dev/null
  sleep 1
  fid=$(docker inspect --format '{{.Id}}' "ev-forced-$round")
  : >"$work/forced-$round.log"
  (
    for _ in $(seq 1 400); do
      sudo -n curl -s --unix-socket "$sock" "http://docker/containers/json?all=1&filters=%7B%22id%22%3A%5B%22$fid%22%5D%7D" |
        jq -r 'if length == 0 then "gone" else .[0].State + " / " + .[0].Status end' >>"$work/forced-$round.log" 2>&1
    done
  ) &
  poll=$!
  sleep 0.2
  docker rm -f "ev-forced-$round" >/dev/null
  wait "$poll"
  echo "evidence: round $round, the list API said, in order, with how many times in a row: $(uniq -c "$work/forced-$round.log" | sed 's/^ *//' | tr '\n' ';')"
done
# The reconciler's own look reads a container in full, which the list isn't: the
# same five rounds, asking for the one container's state.
for round in 1 2 3 4 5; do
  docker run -d --name "ev-forced-full-$round" --label "$label" --restart unless-stopped "$image" >/dev/null
  sleep 1
  fid=$(docker inspect --format '{{.Id}}' "ev-forced-full-$round")
  : >"$work/forced-full-$round.log"
  (
    for _ in $(seq 1 400); do
      sudo -n curl -s --unix-socket "$sock" "http://docker/containers/$fid/json" |
        jq -r 'if .State then .State.Status + " running=" + (.State.Running|tostring) + " dead=" + (.State.Dead|tostring) + " error=" + (.State.Error|tostring) else "gone" end' >>"$work/forced-full-$round.log" 2>&1
    done
  ) &
  poll=$!
  sleep 0.2
  docker rm -f "ev-forced-full-$round" >/dev/null
  wait "$poll"
  echo "evidence: round $round, the container read in full said, in order: $(uniq -c "$work/forced-full-$round.log" | sed 's/^ *//' | tr '\n' ';')"
done
docker run -d --name ev-forced-ps --label "$label" --restart unless-stopped "$image" >/dev/null
sleep 1
: >"$work/forced-ps.log"
(for _ in $(seq 1 60); do docker ps -a --filter name=ev-forced-ps --format '{{.State}} / {{.Status}}' >>"$work/forced-ps.log" 2>&1; done) &
poll=$!
sleep 0.2
docker rm -f ev-forced-ps >/dev/null
wait "$poll"
echo "evidence: docker ps -a said, in order: $(uniq -c "$work/forced-ps.log" | sed 's/^ *//' | tr '\n' ';')"

say "kill the daemon: systemctl kill --signal=SIGKILL docker.service"
t3=$(date +%s)
sudo -n systemctl kill --signal=SIGKILL docker.service
sleep 1
echo "evidence: containers' processes still alive under containerd, a second after the kill: $(pgrep -fc 'nginx: master' || true)"
echo "evidence: docker.service is $(systemctl is-active docker.service), a second after the kill"
sleep 2
sudo -n systemctl reset-failed docker.service 2>/dev/null
sudo -n systemctl start docker.service
wait_docker || exit 1
t4=$(date +%s)
echo "evidence: Docker answered again $((t4 - t3)) s after the kill"

daemon
say "events of the daemon's own, of any kind, since the start ($t0 to $t4)"
docker events --since "$t0" --until "$t4" --filter type=daemon --format '{{.Time}} {{.Type}} {{.Action}}' 2>&1 | sed 's/^/evidence:   event /'
echo "evidence: every event of any kind the returned daemon holds from before its death: $(docker events --since 0 --until "$t3" 2>/dev/null | wc -l)"

say "what the connected client heard, up to the daemon's death and no further"
sed 's/^/evidence:   live /' "$work/live-events.log"
echo "evidence: the connected client's stream $(kill -0 "$events_pid" 2>/dev/null && echo 'is still open' || echo 'ended')"

say "states as soon as Docker answered after the kill"
states
sleep 20
t5=$(date +%s)
say "states 20 s later, once restart policies have had their say"
states
say "events the returned daemon replays from before its death ($t0 to $t3): what it remembers"
events "$t0" "$t3"
say "events since the kill ($t3 to $t5): what the returning daemon said of what it found"
events "$t3" "$t5"

say "for comparison, a clean restart: systemctl restart docker.service"
# shellcheck disable=SC2046 # one ID a word
docker start $(docker ps -aq --filter "label=$label" --filter "name=-left") >/dev/null 2>&1
sleep 3
t6=$(date +%s)
sudo -n systemctl restart docker.service
wait_docker || exit 1
sleep 20
t7=$(date +%s)
say "states 20 s after the clean restart"
states
say "events since the clean restart ($t6 to $t7)"
events "$t6" "$t7"

say "how far back the daemon's events reach: 150 execs, three events each, then count what it replays"
t8=$(date +%s)
for _ in $(seq 1 150); do
  docker exec ev-always-left true >/dev/null 2>&1
done
t9=$(date +%s)
echo "evidence: events replayed for those $((t9 - t8)) s: $(docker events --since "$t8" --until "$((t9 + 1))" 2>/dev/null | wc -l) of about 450 made"
echo "evidence: the oldest it still holds: $(docker events --since 0 --until "$((t9 + 1))" --format '{{.Time}} {{.Action}}' 2>/dev/null | head -1), asked at $t9"

say "done"
