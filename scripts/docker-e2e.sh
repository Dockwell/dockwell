#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Runs built dockwell binaries against this machine's Docker Engine, end to
# end through the CLI: a service from an image runs, changes and is undone;
# the environment exports as a Compose file that docker compose reads and
# lists; a Compose file imports, and two with a service of one name run side
# by side; a service using a variable and a secret runs with their values,
# exports without the secret, and imports again with no change; a tag that moves
# in a local registry is pulled again, rolled back by digest, and survives that
# registry going away; and everything goes again. A file in the shapes real ones
# have runs on a network the server has, with what it asks for really applied,
# and reaches another project on it by name, and a route with no certificate
# reaches it there, as it does a service on the server's own network, by its port
# alone. Each app in the catalog installs as a project of its own and signs in with
# a password no one typed, but for the tunnel, which stops at its plan. A volume,
# PostgreSQL and MySQL each back up, lose their data, and get it back through a
# plan, with owners and modes as they were, and a backup goes sealed to an S3 server and
# comes back. Then the agent build joins the
# full build's control plane, and runs a container on the same Docker. Before
# that, a route gets a real certificate from Pebble, Let's Encrypt's test CA,
# and serves a real container over HTTPS through Caddy, and routes that take no
# certificate serve over plain HTTP on port 80 alone, to what they name. A rollout
# runs under load through that Caddy, with an app slow to turn healthy and one that
# never does, counting every request, and an app and its route go to a new port in
# one change. A container run by hand lists and reads through the CLI, and nothing
# touches it, and one is taken on with no restart, its image changes, and that
# undoes, keeping its volume, environment and network. A tag listed for two
# architectures in that local registry says what ran, and two projects' routes reach
# a service of one name each through one Caddy. One change reaches two servers, with
# the agent build as the second, and one offline is refused by name. The agent's
# reader of package updates runs on real Ubuntu, Debian, AlmaLinux and Alpine. It uses
# throwaway data directories and removes
# what it made, so it never touches a real install. It needs Docker with the compose
# plugin, Go, curl, jq, openssl, pgrep and iproute2's ss, a free port (E2E_WEB_PORT,
# 18080 by default), and ports 80 and 443 free, for the ingress's own part. With
# E2E_PANEL set, as in CI, it also moves the panel's own domain behind Caddy and
# back, which needs sudo to let this user bind ports 80 and 443. With E2E_ROOT set,
# as in CI, Dockwell runs as root through sudo, as it does on a server, so Caddy runs
# as its own user and its directories are given to it, and it restarts Docker itself
# along the way.
#
#   scripts/docker-e2e.sh bin/dockwell bin/dockwell-agent
set -euo pipefail
# Anything this starts asks nothing beyond this machine (DOCKWELL_TEST, ADR 0034).
export DOCKWELL_TEST=1

usage="usage: scripts/docker-e2e.sh path/to/dockwell path/to/dockwell-agent"
for tool in docker curl go jq openssl pgrep ss; do
  command -v "$tool" >/dev/null || {
    echo "docker-e2e: $tool isn't installed, and the script needs it. Install it, such as with apt-get install $tool, then run it again" >&2
    exit 1
  }
done
bin=$(realpath "${1:?$usage}")
agent=$(realpath "${2:?$usage}")
addr=127.0.0.1:${E2E_PORT:-7697}
web_port=${E2E_WEB_PORT:-18080}
data=$(mktemp -d)
agent_data=$(mktemp -d)
log=$(mktemp)
agent_log=$(mktemp)
project=""
extra_projects=""
real_projects=()
agent_pid=""
moving_dir=""
moving_repo=""
digest_first=""
digest_second=""
digest_third=""
built=""
built_next=""
multi_repo=""
# A registry, by digest, for the images of the step about a tag that moves.
registry_image=registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373

port=${addr##*:}
cookie=$(mktemp)
passkey=$(mktemp)
stepup=$(mktemp)
# What a file's relative paths start from. Not the data directory, which no
# container may mount, and Docker makes what's in it as root.
real_dir=$(mktemp -d)

load_pid=""
rollout_dir=$(mktemp -d)
pebble_dir=$(mktemp -d)
tools=${E2E_INGRESS_TOOLS:-$(mktemp -d)}
pebble_pids=()

# as_dockwell is what runs a command as whoever Dockwell runs as, and priv does so
# for what touches its files. A function run in the background is a subshell, whose
# PID isn't the command's, so start_dockwell uses the array.
as_dockwell=()
[[ -z "${E2E_ROOT:-}" ]] || as_dockwell=(sudo -n)
priv() { "${as_dockwell[@]}" "$@"; }

# The ingress's certificates come from Pebble, on this machine.
export DOCKWELL_ACME_DIRECTORY_URL=https://localhost:14000/dir
export DOCKWELL_ACME_ROOTS=$pebble_dir/cert.pem
# A release newer than the one installed comes from here, which Dockwell reads each time.
catalog_dir=$(mktemp -d)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 -subj /CN=localhost \
  -addext subjectAltName=DNS:localhost,IP:127.0.0.1 -keyout "$pebble_dir/key.pem" -out "$DOCKWELL_ACME_ROOTS" 2>/dev/null

start_dockwell() {
  # SSL_CERT_FILE is for the panel's own certificates alone, as they come through
  # Go's default roots. sudo keeps none of these unless it is told.
  "${as_dockwell[@]}" env DOCKWELL_TEST=1 DOCKWELL_CATALOG_DIR="$catalog_dir" SSL_CERT_FILE="$DOCKWELL_ACME_ROOTS" DOCKWELL_ACME_DIRECTORY_URL="$DOCKWELL_ACME_DIRECTORY_URL" DOCKWELL_ACME_ROOTS="$DOCKWELL_ACME_ROOTS" \
    "$bin" run --listen "$addr" --agent-listen 127.0.0.1:0 --data-dir "$data" --log-format json "$@" >>"$log" 2>&1 &
  pid=$!
}
start_dockwell
# moving_tidy removes what the step about a tag that moves brought here.
moving_tidy() {
  docker rm --force --volumes e2e-registry >/dev/null 2>&1 || true
  [[ -z "$built" ]] || docker rmi "$built" >/dev/null 2>&1 || true
  [[ -z "$built_next" ]] || docker rmi "$built_next" >/dev/null 2>&1 || true
  [[ -z "$multi_repo" ]] || docker rmi "$multi_repo:1" "$multi_repo:one" >/dev/null 2>&1 || true
  docker rmi e2e-moving:first e2e-moving:second e2e-moving:third "$moving_repo:latest" "$moving_repo@$digest_first" "$moving_repo@$digest_second" "$moving_repo@$digest_third" >/dev/null 2>&1 || true
  [[ -z "$moving_dir" ]] || rm -rf "$moving_dir"
  moving_dir=""
}
# restore_daemon puts back Docker's settings, if this changed them, and restarts it.
daemon_changed=""
daemon_before=""
restore_daemon() {
  [[ -n "$daemon_changed" ]] || return 0
  daemon_changed=""
  if [[ -n "$daemon_before" ]]; then
    printf '%s\n' "$daemon_before" | priv tee /etc/docker/daemon.json >/dev/null
  else
    priv rm -f /etc/docker/daemon.json
  fi
  priv systemctl restart docker
}
cleanup() {
  restore_daemon || true
  docker image rm e2e-wrong-arch:1 >/dev/null 2>&1 || true
  for p in $project $extra_projects; do
    docker compose -p "$p" down --volumes --remove-orphans --timeout 1 >/dev/null 2>&1 || true
  done
  for p in "${real_projects[@]}"; do
    docker compose -p "$p" down --volumes --remove-orphans --timeout 1 >/dev/null 2>&1 || true
  done
  docker image rm e2e-local-only:1 >/dev/null 2>&1 || true
  docker ps --all --quiet --filter label=dev.dockwell.volume-tool | xargs --no-run-if-empty docker rm --force >/dev/null 2>&1 || true
  docker rm --force e2e-s3 e2e-by-hand e2e-take-on e2e-on-bridge e2e-sidecar e2e-routed e2e-agent-web e2e-two-own e2e-two-agent e2e-holds-80 e2e-returns e2e-on-server e2e-not-started e2e-oom dockwell-ingress dockwell-ingress-keep >/dev/null 2>&1 || true
  docker volume rm e2e-by-hand-data e2e-take-on-data >/dev/null 2>&1 || true
  docker network rm e2e-by-hand-net >/dev/null 2>&1 || true
  moving_tidy
  docker network rm dockwell_ingress e2e-took-subnet e2e-edge e2e-lan e2e-tunnel e2e-other >/dev/null 2>&1 || true
  [[ -z "$agent_pid" ]] || kill "$agent_pid" 2>/dev/null || true
  [[ -z "$load_pid" ]] || kill "$load_pid" 2>/dev/null || true
  priv kill "$pid" 2>/dev/null || true
  for p in "${pebble_pids[@]:-}"; do
    [[ -z "$p" ]] || kill "$p" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  docker image rm --force dockwell-e2e-app:slow dockwell-e2e-app:never >/dev/null 2>&1 || true
  priv rm -rf "$log" "$agent_log" "$cookie" "$passkey" "$stepup" "$data" "$agent_data" "$pebble_dir" "$catalog_dir" "$real_dir" "$rollout_dir"
}
trap cleanup EXIT

# ingress_state says all there is to know of the ingress, as most of what can
# go wrong with it is on real Docker, where nobody can run it but CI.
ingress_state() {
  echo "--- the ingress's container" >&2
  docker ps -a --filter name=dockwell-ingress --format '{{.Names}} {{.Image}} {{.Status}} {{.Ports}}' >&2 || true
  docker inspect --format 'network {{.HostConfig.NetworkMode}}, user {{.Config.User}}, restart {{.HostConfig.RestartPolicy.Name}}, exit {{.State.ExitCode}}, error {{.State.Error}}' dockwell-ingress >&2 2>/dev/null || true
  docker ps -a --filter name=dockwell-ingress-keep --format '{{.Names}} {{.Image}} {{.Status}}' >&2 || true
  echo "--- its log" >&2
  docker logs --tail 60 dockwell-ingress >&2 2>&1 || true
  echo "--- the ingress network" >&2
  docker network inspect --format '{{.Name}} internal={{.Internal}} {{json .IPAM.Config}} {{json .Containers}} {{json .Options}}' dockwell_ingress >&2 2>/dev/null || true
  echo "--- what certificates are doing" >&2
  priv tail -n 40 "$data/ingress/data/dockwell-tls.log" >&2 2>/dev/null || true
  echo "--- addresses" >&2
  priv cat "$data/ingress/addresses.json" >&2 2>/dev/null || true
  echo "--- who is on ports 80 and 443" >&2
  priv ss -ltnup 'sport = :80 or sport = :443' >&2 2>/dev/null || true
  echo "--- domains, as the API has them" >&2
  dw domains list --json >&2 2>/dev/null || true
  echo "--- Pebble" >&2
  tail -n 30 "$pebble_dir/pebble.out" >&2 2>/dev/null || true
}
in_ingress=""

# rollout_state says where the rollout under test stands, for a failure in that
# part: what Docker runs, what the API says of it, what changed, and what the load
# has counted so far.
rollout_state() {
  echo "--- the app's containers, newest first" >&2
  docker ps -a --filter "label=com.docker.compose.service=app" --format '{{.Names}} {{.State}} {{.Status}} {{.Image}} {{.Label "com.docker.compose.container-number"}}' >&2 || true
  echo "--- its health, as the API has it" >&2
  dw health list --service app --json >&2 2>/dev/null || true
  echo "--- the last changes, newest first" >&2
  dw changes list --json 2>/dev/null | jq -c '.changes[:6][]? | {id, summary, state, error, because, restart, rolledBackBy}' >&2 || true
  echo "--- the app's last events, newest first" >&2
  dw events list --page-size 12 --json 2>/dev/null | jq -c '.events[]? | {time, action, summary, health}' >&2 || true
  echo "--- what Caddy dials for it" >&2
  priv cat "$data/ingress/run/upstreams/dockwell.$project.roll" >&2 2>/dev/null || true
  echo >&2
  if [[ -n "$load_pid" ]]; then
    echo "--- the load is still running, so what it counted isn't here" >&2
  elif [[ -s "$rollout_dir/load.json" ]]; then
    echo "--- what the load counted" >&2
    cat "$rollout_dir/load.json" >&2
    echo >&2
  fi
}
in_rollout=""

fail() {
  echo "docker-e2e: $*" >&2
  [[ -z "$in_rollout" ]] || rollout_state
  [[ -z "$in_ingress" ]] || ingress_state
  echo "--- dockwell log" >&2
  cat "$log" >&2
  if [[ -s "$agent_log" ]]; then
    echo "--- the agent build's log" >&2
    cat "$agent_log" >&2
  fi
  exit 1
}

# Local access on the control server: the CLI finds the control plane, and
# makes its own token, from the data directory.
dw() { priv env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text "$bin" "$@"; }
step() { echo "--- $*"; }
# says PATTERN COMMAND... reports a command that ran, and printed something that
# holds PATTERN. It captures what it printed first, as grep -q stops reading at its
# first match: under pipefail what still writes to it can fail on the closed pipe,
# so a check that passed reads as failed, and in an if, one that should fail passes.
says() {
  local out
  out=$("${@:2}") || return 1
  grep -qE -- "$1" <<<"$out"
}

for _ in $(seq 100); do
  curl -fsS "http://$addr/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
[[ "$(curl -fsS "http://$addr/healthz")" == "ok" ]] || fail "/healthz did not answer ok within 10 s"
for _ in $(seq 300); do
  grep -q 'connected to the control plane' "$log" && break
  sleep 0.1
done
grep -q 'connected to the control plane' "$log" || fail "the control server's own agent didn't connect within 30 s"
if grep -q "the agent can't run some kinds" "$log"; then
  fail "the agent can't run the Docker kinds; is Docker running?"
fi

# Local access acts for the install's owner, so claim it first, with a
# software passkey, as the panel would. Local access is read-only until
# dockwell login --local lets it make changes.
# dockwell run prints no link here, as its output isn't a terminal, and says to
# run dockwell claim, which does.
if grep -q '/claim#' "$log"; then
  fail "dockwell run printed a claim link where it isn't a terminal"
fi
grep -q 'To claim it, run this on this server' "$log" || fail "dockwell run didn't say how to get a claim link"
claim=$(dw claim) || fail "dockwell claim failed"
link=$(grep -o 'http://localhost:[0-9]*/claim#[A-Za-z0-9_-]*' <<<"$claim" | head -1) || true
[[ -n "$link" ]] || fail "dockwell claim printed no claim link: $claim"
(cd "$(dirname "$0")/.." && go run ./scripts/claim "$link" "$cookie" "$passkey" && go build -o "$stepup" ./scripts/stepup) || fail "can't claim the install"
dw login --local >/dev/null || fail "dockwell login --local failed"

# person_call calls the API as a signed-in person does, from the panel's origin,
# for what no token may do.
person_call() {
  curl -fsS --max-time 30 --resolve "localhost:$port:127.0.0.1" -H "Origin: http://localhost:$port" -H 'Content-Type: application/json' \
    -H "Cookie: $(cat "$cookie")" -d "$2" "http://localhost:$port/dockwell.v1.$1"
}
# person_answer is person_call that passes on what a refusal says.
person_answer() {
  curl -sS --max-time 30 --resolve "localhost:$port:127.0.0.1" -H "Origin: http://localhost:$port" -H 'Content-Type: application/json' \
    -H "Cookie: $(cat "$cookie")" -d "$2" "http://localhost:$port/dockwell.v1.$1"
}
# change_field reads a field of a change.
change_field() { dw changes get "$1" --json | jq -r ".change.$2"; }
# wait_applied waits for a change a person applied.
wait_applied() {
  local state=""
  for _ in $(seq 240); do
    state=$(change_field "$1" state) || state=""
    [[ "$state" == CHANGE_STATE_APPLIED ]] && return 0
    [[ "$state" == CHANGE_STATE_FAILED ]] && fail "$1 failed: $(dw changes get "$1" --json)"
    sleep 0.5
  done
  fail "$1 is $state after 120 s, want applied"
}
# take_on has a person plan and apply taking a container on as a service, and checks
# nothing about it moved. A refusal says why, in what the job prints. It sets taken_plan
# to the answer.
taken_plan=""
take_on() {
  local started
  started=$(touched "$1") || fail "docker inspect $1 failed"
  taken_plan=$(person_answer ContainerService/AdoptContainer "$(jq -nc --arg id "$1" --arg service "$2" '{id: $id, service: $service}')") ||
    fail "a person can't reach the API to take on $1"
  jq -e '.change.id' <<<"$taken_plan" >/dev/null || fail "taking on $1 was refused: $taken_plan"
  [[ "$taken_plan" != *not-in-any-plan* ]] || fail "the plan to take on $1 shows a value of its environment: $taken_plan"
  person_apply "$(jq -r '.change.id' <<<"$taken_plan")"
  [[ "$(touched "$1")" == "$started" ]] || fail "taking it on touched $1: $started, then $(touched "$1")"
}

# person_apply applies a planned change as a person does in the panel, and waits
# for it. It fails on one that takes a passkey: that's person_apply_root, so a plan
# that's as good as root by mistake shows here.
person_apply() {
  local fingerprint
  fingerprint=$(change_field "$1" plan.fingerprint) || fail "can't read the plan of $1"
  person_call ChangeService/ApplyChange "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\"}" >/dev/null || fail "a person can't apply $1"
  wait_applied "$1"
}
# person_apply_root applies a plan that's as good as root, as the panel does: the
# server refuses it with no passkey, and takes it with a proof from a real step-up,
# with the passkey the claim made, for this change alone.
person_apply_root() {
  local fingerprint answer proof
  fingerprint=$(change_field "$1" plan.fingerprint) || fail "can't read the plan of $1"
  answer=$(curl -sS --max-time 30 -i -w '\n%{http_code}' --resolve "localhost:$port:127.0.0.1" -H "Origin: http://localhost:$port" -H 'Content-Type: application/json' \
    -H "Cookie: $(cat "$cookie")" -d "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\"}" "http://localhost:$port/dockwell.v1.ChangeService/ApplyChange") ||
    fail "the API didn't answer a person applying $1"
  [[ "${answer##*$'\n'}" == 403 ]] || fail "a person applied $1, which is as good as root, with no passkey: $answer"
  grep -qi '^dockwell-error: needs_step_up' <<<"$answer" || fail "the server refused a person applying $1 for another reason: $answer"
  [[ "$(change_field "$1" state)" == CHANGE_STATE_PLANNED ]] || fail "$1 isn't planned after it was refused"
  proof=$("$stepup" "http://localhost:$port" "$cookie" "$passkey" STEP_UP_ACTION_APPLY_ROOT_CHANGE "$1") || fail "can't confirm $1 with the passkey"
  person_call ChangeService/ApplyChange "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\",\"proof\":\"$proof\"}" >/dev/null || fail "a person can't apply $1 with its passkey"
  wait_applied "$1"
}
# token_refused checks that the server itself refuses a token applying a change,
# through its API, rather than the CLI's own check that stops short of asking. The
# token is the one that check protects: the read-write token dockwell login --local
# keeps, an API token like any other to the server. Each dw --yes before this has
# applied a plan with it, so a refusal is about this plan, and not the token.
token_refused() {
  local token fingerprint answer
  token=$(priv cat "$data/control/local-token.json" | jq -r .token) || fail "can't read the token dockwell login --local keeps"
  [[ "$token" == dwt_* ]] || fail "the token dockwell login --local keeps isn't an API token: ${token:0:8}"
  fingerprint=$(change_field "$1" plan.fingerprint) || fail "can't read the plan of $1"
  answer=$(curl -sS --max-time 30 -i -w '\n%{http_code}' -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -d "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\"}" "http://$addr/dockwell.v1.ChangeService/ApplyChange") || fail "the API didn't answer a token applying $1"
  [[ "${answer##*$'\n'}" == 403 ]] || fail "the server let a token apply $1: $answer"
  grep -qi '^dockwell-error: needs_session' <<<"$answer" || fail "the server refused a token applying $1 for another reason: $answer"
  [[ "$(change_field "$1" state)" == CHANGE_STATE_PLANNED ]] || fail "$1 isn't planned after the token was refused"
}

ports="{\"hostPort\":$web_port,\"containerPort\":80,\"hostIp\":\"127.0.0.1\"}"

# service_container says what Docker runs for the service $2 of the Compose
# project $1, and there is one, by the labels Dockwell and Compose give it,
# whatever it's named: Dockwell names it default.production.web-1, and one docker
# compose makes is env_…-web-1 (ADR 0035). Nothing here builds a name by hand.
service_container() {
  local out
  out=$(docker ps --filter "label=com.docker.compose.project=$1" --filter "label=com.docker.compose.service=$2" --format '{{.Names}}') ||
    fail "docker ps failed"
  [[ -n "$out" && "$out" != *$'\n'* ]] || fail "Docker runs '$out' for $2 in $1, want one container"
  echo "$out"
}
# named_container is service_container in the default environment's project.
named_container() { service_container "$project" "$1"; }

step "Package updates read with each distribution's own tool: apt on Ubuntu and Debian, dnf on AlmaLinux, apk on Alpine"
# The agent's reader, built on its own, runs as root in a container of each, and
# refreshes the lists itself (ADR 0043). On Ubuntu, Debian and AlmaLinux a package is
# put back to an older version first, and the step fails where none can be, as one
# that put nothing back would prove nothing. Then what the reader lists, and marks a
# security fix, has to be what the tool itself says, in the same container.
updates_bin="$rollout_dir/updates"
(cd "$(dirname "$0")/.." && CGO_ENABLED=0 go build -o "$updates_bin" ./scripts/updates) || fail "can't build scripts/updates"
# Each distribution has two scripts. prepare-* puts a package back to an older
# version its repositories still have, where it can, and says which. after-* runs once the reader
# has read, which refreshed the metadata first, so what the tool itself lists is
# from the metadata the reader read. Lines that start "raw " are what the tool
# printed as it was, for a failure to show.
cat >"$rollout_dir/prepare-apt.sh" <<'SH'
set -e
export DEBIAN_FRONTEND=noninteractive
apt-get -qq update >/dev/null
for p in less tzdata tar gzip bash libssl3 libgnutls30 libsystemd0 libudev1 gpgv libsqlite3-0; do
  count=$(apt-cache madison "$p" | awk -F'|' '{ gsub(/ /, "", $2); print $2 }' | sort -u | wc -l)
  [ "$count" -ge 2 ] || continue
  old=$(apt-cache madison "$p" | awk -F'|' '{ gsub(/ /, "", $2); v = $2 } END { print v }')
  apt-get -qq install -y --allow-downgrades "$p=$old" >/dev/null 2>&1 || continue
  [ "$(dpkg-query -W -f '${Version}' "$p" 2>/dev/null)" = "$old" ] || continue
  echo "put-back $p $old" >&2
  break
done
SH
cat >"$rollout_dir/after-apt.sh" <<'SH'
apt-get -s --with-new-pkgs upgrade | awk '/^Inst / { print "name " $2 } /^Inst .*-security/ { print "security " $2 }'
SH
cat >"$rollout_dir/prepare-dnf.sh" <<'SH'
set -e
arch=$(uname -m)
for p in less tar which file zip unzip tzdata; do
  # Every version the repositories have, the installed one included, oldest first.
  vers=$(dnf -q repoquery --arch "$arch,noarch" --qf '%{evr}' "$p" 2>/dev/null | sort -uV)
  [ "$(printf '%s\n' "$vers" | grep -c . || true)" -ge 2 ] || continue
  old=$(printf '%s\n' "$vers" | head -n 1)
  dnf -q -y install "$p-${old#*:}" >/dev/null 2>&1 || dnf -q -y downgrade "$p-${old#*:}" >/dev/null 2>&1 || continue
  now=$(rpm -q --qf '%|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE}\n' "$p" 2>/dev/null | tail -n 1)
  [ "${now#*:}" = "${old#*:}" ] || continue
  echo "put-back $p $now" >&2
  echo "raw $p put back to $now at $(date -u '+%F %T'), of: $(printf '%s ' $vers)" >&2
  echo "$p" >/tmp/put-back
  break
done
code=0
dnf -q --cacheonly check-update >/tmp/before 2>&1 || code=$?
echo "raw Before the reader, dnf -q --cacheonly check-update exited $code and printed:" >&2
sed 's/^/raw   /' /tmp/before >&2
SH
cat >"$rollout_dir/after-dnf.sh" <<'SH'
raw() { sed 's/^/raw   /'; }
code=0
dnf -q --cacheonly check-update >/tmp/listed 2>/tmp/listed-err || code=$?
echo "raw After the reader, dnf -q --cacheonly check-update exited $code and printed:"
cat /tmp/listed /tmp/listed-err | raw
awk '/^Obsoleting/ { exit } NF == 1 && /\./ { n = $1; next } NF == 2 && n != "" { print "name " n; n = ""; next } NF == 3 { print "name " $1 }' /tmp/listed
dnf -q --cacheonly updateinfo list --security 2>/dev/null | awk 'NF >= 3 { print "advisory " $3 }'
p=$(cat /tmp/put-back 2>/dev/null || true)
if [ -n "$p" ]; then
  echo "raw rpm -q $p: $(rpm -q "$p" 2>&1)"
  echo "raw dnf -q --cacheonly --showduplicates list $p:"
  dnf -q --cacheonly --showduplicates list "$p" 2>&1 | raw
fi
echo "raw The metadata the reader read, at $(date -u '+%F %T'):"
stat -c '%y %n' /var/cache/dnf/*/repodata/repomd.xml 2>&1 | raw
SH
cat >"$rollout_dir/prepare-apk.sh" <<'SH'
apk -q update
SH
cat >"$rollout_dir/after-apk.sh" <<'SH'
apk version -l '<' | awk 'NF == 3 && $2 == "<" { print "name " $1 }'
SH
# read_updates IMAGE KIND runs prepare-KIND, the reader, then after-KIND, in a
# container of IMAGE, with what the scripts say in $rollout_dir/said and the reader's
# JSON on standard output. It runs in a command substitution, so its caller fails
# for it, by the reader's exit.
read_updates() {
  docker run --rm -v "$updates_bin:/usr/local/bin/dockwell-updates:ro" \
    -v "$rollout_dir/prepare-$2.sh:/prepare.sh:ro" -v "$rollout_dir/after-$2.sh:/after.sh:ro" "$1" \
    sh -c 'sh /prepare.sh || exit; dockwell-updates; code=$?; sh /after.sh >&2; exit $code' 2>"$rollout_dir/said"
}
said() { cat "$rollout_dir/said"; }
# raw prints what the tool printed as it was, after a blank line, or nothing.
raw() {
  local printed
  printed=$(sed -n 's/^raw //p' "$rollout_dir/said")
  [[ -z "$printed" ]] || printf '\n%s' "$printed"
}
# the_tool_says KIND prints what the scripts said of KIND, one a line, sorted.
the_tool_says() { sed -n "s/^$1 //p" "$rollout_dir/said" | sort -u; }
# the_reader_says JSON JQ prints the names the reader gave, by JQ, sorted.
the_reader_says() { jq -r "$2" <<<"$1" | sort -u; }
# put_back DISTRO JSON checks the package the script put back is listed, at the
# version it was put back to, with a newer one available.
put_back() {
  local put pkg version
  put=$(sed -n 's/^put-back //p' "$rollout_dir/said")
  [[ -n "$put" ]] || fail "on $1, nothing could be put back to an older version, so the step proves nothing: $(said)"
  pkg=${put%% *}
  version=${put#* }
  jq -e --arg p "$pkg" --arg v "$version" '.packages | any(.name == $p and .installed == $v and .available != $v)' >/dev/null <<<"$2" ||
    fail "on $1, $pkg put back to $version isn't listed as an update: $2$(raw)"
}
for distro in "Ubuntu ubuntu:24.04" "Debian debian:12"; do
  name=${distro%% *}
  out=$(read_updates "${distro#* }" apt) || fail "the agent's reader failed on $name: $(said)"
  jq -e '.manager == "PACKAGE_MANAGER_APT" and .marksSecurity and (.unavailable // "") == "" and (.refreshError // "") == "" and .listsTime != null' >/dev/null <<<"$out" ||
    fail "on $name, the reader said: $out"
  put_back "$name" "$out"
  diff <(the_reader_says "$out" '.packages[]?.name') <(the_tool_says name) ||
    fail "on $name, the reader listed other packages than apt-get --simulate does"
  diff <(the_reader_says "$out" '.packages[]? | select(.security) | .name') <(the_tool_says security) ||
    fail "on $name, the reader marked other security fixes than apt-get --simulate does"
done
alma=$(read_updates almalinux:9 dnf) || fail "the agent's reader failed on AlmaLinux: $(said)"
jq -e '.manager == "PACKAGE_MANAGER_DNF" and .marksSecurity and (.refreshError // "") == "" and .listsTime != null' >/dev/null <<<"$alma" ||
  fail "on AlmaLinux, the reader said: $alma$(raw)"
put_back AlmaLinux "$alma"
diff <(the_reader_says "$alma" '.packages[]? | .name + "." + .architecture') <(the_tool_says name) ||
  fail "on AlmaLinux, the reader listed other packages than dnf check-update does:$(raw)"
# A security fix is a package dnf lists that an advisory names, by its name.
advised=$(the_tool_says advisory | sed -E 's/-[^-]+-[^-]+$//' | sort -u)
listed=$(the_tool_says name | sed -E 's/\.[^.]+$//' | sort -u)
diff <(the_reader_says "$alma" '.packages[]? | select(.security) | .name') <(comm -12 <(echo "$advised") <(echo "$listed")) ||
  fail "on AlmaLinux, the reader marked other security fixes than dnf updateinfo names:$(raw)"
alpine=$(read_updates alpine:3.20 apk) || fail "the agent's reader failed on Alpine: $(said)"
jq -e '.manager == "PACKAGE_MANAGER_APK" and (.marksSecurity // false) == false and (.refreshError // "") == "" and .listsTime != null' >/dev/null <<<"$alpine" ||
  fail "on Alpine, the reader said: $alpine"
diff <(the_reader_says "$alpine" '.packages[]? | .name + "-" + .installed') <(the_tool_says name) ||
  fail "on Alpine, the reader listed other packages than apk version does"

step "A service from an image runs"
dw services create web --app-image nginx:1.27-alpine --app-ports "$ports" --app-env GREETING=hello --yes ||
  fail "services create failed"
container=$(docker ps --filter label=com.docker.compose.service=web --format '{{.Names}}') || fail "docker ps failed"
# docker ps says what it is, by its project and environment, though its Compose
# project is still the environment's ID, which never changes (ADR 0035).
readable=default.production
[[ "$container" == "$readable.web-1" ]] || fail "Docker runs '$container' for web, want $readable.web-1, named after its project and environment"
project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$container") ||
  fail "docker inspect $container failed"
[[ "$project" == env_* ]] || fail "$container is in the Compose project '$project', want its environment's, env_…"
says nginx curl -fsS --max-time 10 "http://127.0.0.1:$web_port/" || fail "web doesn't serve on port $web_port"
env_of() { docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container"; }
says '^GREETING=hello$' env_of || fail "web's environment is $(env_of)"

step "Its logs read through the CLI, and follow, and stop on an interrupt"
logs=$(dw logs read web --page-size 50) || fail "logs read failed"
grep -q 'nginx\|start' <<<"$logs" || fail "logs read printed no line of nginx's: $logs"
# Into a variable first: under pipefail, grep -q leaving early can fail what writes to it.
listing=$(dw logs sources web) || fail "logs sources failed"
grep -q "Running" <<<"$listing" || fail "logs sources doesn't list web's container as running: $listing"
follow_log=$(mktemp)
# Not through the shell function dw: a function in the background is a subshell,
# which an interrupt doesn't reach the binary through. Nor by interrupting sudo,
# which doesn't pass on a signal from its own process group, as a script's is. So
# it says its own process ID as its first line, and keeps that ID as it becomes
# the binary, and the interrupt goes straight to that.
"${as_dockwell[@]}" env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text bash -c 'echo "$$"; exec "$@"' follow \
  "$bin" logs follow web --tail 1 >"$follow_log" 2>&1 &
follower=$!
# A request nginx logs, which reaches a follower within a second or two.
for _ in $(seq 50); do
  (($(wc -l <"$follow_log") >= 2)) && break
  sleep 0.1
done
follower_pid=$(head -n 1 "$follow_log" | tr -d '\r')
[[ "$follower_pid" =~ ^[0-9]+$ ]] || fail "dockwell logs follow didn't start: $(cat "$follow_log")"
curl -fsS --max-time 10 "http://127.0.0.1:$web_port/e2e-follows-this" >/dev/null 2>&1 || true
curl -fsS --max-time 10 "http://127.0.0.1:$web_port/" >/dev/null
for _ in $(seq 50); do
  grep -q 'e2e-follows-this' "$follow_log" && break
  sleep 0.1
done
grep -q 'e2e-follows-this' "$follow_log" || fail "dockwell logs follow didn't print a line within 5 s: $(cat "$follow_log")"
priv kill -INT "$follower_pid" || fail "can't interrupt dockwell logs follow: $(cat "$follow_log")"
# By /proc rather than kill -0, which can't tell a process that has gone from one
# this user may not signal, and would read sudo's as having stopped.
for _ in $(seq 100); do
  [[ -d "/proc/$follower" ]] || break
  sleep 0.1
done
if [[ -d "/proc/$follower" ]]; then
  priv kill -KILL "$follower_pid" || true
  fail "dockwell logs follow didn't stop within 10 s of an interrupt: $(cat "$follow_log")"
fi
code=0
wait "$follower" || code=$?
((code == 0)) || fail "dockwell logs follow exited $code on an interrupt: $(cat "$follow_log")"
rm -f "$follow_log"
docker inspect --format '{{.HostConfig.LogConfig.Type}} {{index .HostConfig.LogConfig.Config "max-size"}}' "$container" | grep -qx 'json-file 10m' ||
  fail "web's log isn't rotated: $(docker inspect --format '{{json .HostConfig.LogConfig}}' "$container")"
docker inspect --format '{{index .Config.Labels "dev.dockwell.revision"}}' "$container" | grep -qx '[0-9][0-9]*' ||
  fail "web's container doesn't say which revision made it"

step "A container run by hand is listed and read, Dockwell's own isn't, and looking touches neither"
# The value is in its environment, a label and its command, none of which a list holds.
docker run --detach --name e2e-by-hand --env E2E_BY_HAND=not-for-a-list --label e2e.note=not-for-a-list --publish 127.0.0.1:18095:80 \
  --volume e2e-by-hand-data:/data nginx:1.27-alpine sh -c ': not-for-a-list; echo e2e-by-hand-says-hello; exec nginx -g "daemon off;"' >/dev/null ||
  fail "docker run e2e-by-hand failed"
touched() { docker inspect --format '{{.Id}} {{.RestartCount}} {{.State.StartedAt}}' "$1"; }
by_hand_before=$(touched e2e-by-hand) || fail "docker inspect e2e-by-hand failed"
web_before=$(touched "$container") || fail "docker inspect $container failed"
listed=$(dw containers list --json) || fail "containers list failed"
[[ "$listed" != *not-for-a-list* ]] || fail "the list shows a value of a container's environment, labels or command: $listed"
by_hand=$(jq -c '.containers[] | select(.name == "e2e-by-hand")' <<<"$listed") || fail "containers list printed what isn't JSON: $listed"
[[ -n "$by_hand" ]] || fail "the list has no e2e-by-hand: $listed"
jq -e '.image == "nginx:1.27-alpine" and .state == "CONTAINER_STATE_RUNNING"
  and (.ports[0] | .serverPort == 18095 and .containerPort == 80 and .address == "127.0.0.1")
  and (.mounts[0] | .volume == "e2e-by-hand-data" and .target == "/data")' <<<"$by_hand" >/dev/null ||
  fail "e2e-by-hand lists as $by_hand"
own=$(jq -r '[.containers[].name] | index("'"$container"'") // "not listed"' <<<"$listed")
[[ "$own" == "not listed" ]] || fail "Dockwell's own $container is in the list of what it didn't make: $listed"
jq -e '.ownContainers >= 1' <<<"$listed" >/dev/null || fail "the list doesn't say it left out Dockwell's own: $listed"
server_name=$(dw servers list --json | jq -r '.servers[0].name') || fail "servers list failed"
for _ in $(seq 50); do
  by_hand_log=$(dw logs read e2e-by-hand --server "$server_name") || fail "logs read of e2e-by-hand failed"
  [[ "$by_hand_log" == *e2e-by-hand-says-hello* ]] && break
  sleep 0.1
done
[[ "$by_hand_log" == *e2e-by-hand-says-hello* ]] || fail "e2e-by-hand's log reads: $by_hand_log"
own_log=$(dw logs read "$container" --server "$server_name" 2>&1 || true)
[[ "$own_log" == *"which Dockwell runs"* ]] || fail "reading Dockwell's own container as one it didn't make said: $own_log"
[[ "$(touched e2e-by-hand)" == "$by_hand_before" ]] || fail "looking at e2e-by-hand touched it: $by_hand_before, then $(touched e2e-by-hand)"
[[ "$(touched "$container")" == "$web_before" ]] || fail "looking at e2e-by-hand touched $container"

step "A follow goes on when its container is made again under its name"
# followed_through NAME SAYS COMMAND... follows a container's log while COMMAND makes
# it again, and holds that what the new one says arrives, with a marker where the old
# one's lines stop. It stops the follow as the step above does, for the same reasons.
followed_through() {
  local name=$1 says=$2 follow_log follower follower_pid
  shift 2
  follow_log=$(mktemp)
  "${as_dockwell[@]}" env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text bash -c 'echo "$$"; exec "$@"' follow \
    "$bin" logs follow "$name" --server "$server_name" --tail 5 --json >"$follow_log" 2>&1 &
  follower=$!
  for _ in $(seq 100); do
    (($(wc -l <"$follow_log") >= 2)) && break
    sleep 0.1
  done
  follower_pid=$(head -n 1 "$follow_log" | tr -d '\r')
  [[ "$follower_pid" =~ ^[0-9]+$ ]] || fail "dockwell logs follow $name didn't start: $(cat "$follow_log")"
  "$@" || fail "making $name again failed: $*"
  for _ in $(seq 300); do
    grep -q "$says" "$follow_log" && break
    sleep 0.1
  done
  grep -q "$says" "$follow_log" || fail "following $name, what the new one says never came: $(cat "$follow_log")"
  for _ in $(seq 100); do
    grep -q LOG_GAP_REASON_GONE "$follow_log" && break
    sleep 0.1
  done
  grep -q LOG_GAP_REASON_GONE "$follow_log" || fail "following $name, no marker says the old one's lines stop: $(cat "$follow_log")"
  priv kill -INT "$follower_pid" || fail "can't interrupt dockwell logs follow: $(cat "$follow_log")"
  wait "$follower" || true
  rm -f "$follow_log"
}
by_hand_again() {
  docker rm --force e2e-by-hand >/dev/null &&
    docker run --detach --name e2e-by-hand nginx:1.27-alpine sh -c 'echo e2e-by-hand-made-again; exec nginx -g "daemon off;"' >/dev/null
}
followed_through e2e-by-hand e2e-by-hand-made-again by_hand_again
real_projects+=(e2e-remade)
remade_file=$(mktemp)
cat >"$remade_file" <<'EOF_COMPOSE'
services:
  app:
    image: nginx:1.27-alpine
    command: ["sh", "-c", "echo e2e-turn-${E2E_TURN}; exec nginx -g 'daemon off;'"]
EOF_COMPOSE
compose_turn() { E2E_TURN=$1 docker compose -p e2e-remade -f "$remade_file" up --detach --wait >/dev/null 2>&1; }
compose_turn first || fail "docker compose up failed"
followed_through e2e-remade-app-1 e2e-turn-second compose_turn second
docker compose -p e2e-remade -f "$remade_file" down --volumes --timeout 1 >/dev/null 2>&1 || fail "docker compose down failed"
rm -f "$remade_file"
docker rm --force e2e-by-hand >/dev/null || fail "docker rm e2e-by-hand failed"
docker volume rm e2e-by-hand-data >/dev/null || fail "docker volume rm e2e-by-hand-data failed"

step "A container run by hand is taken on with no restart, its image changes with a plan, and that undoes"
# On a network of its own, with a port, a volume holding what it wrote, and a value
# nothing may show, as the container someone moved here runs.
docker network create e2e-by-hand-net >/dev/null || fail "docker network create failed"
docker run --detach --name e2e-take-on --restart always --network e2e-by-hand-net --env E2E_TAKEN=not-in-any-plan \
  --publish 127.0.0.1:18096:80 --volume e2e-take-on-data:/data nginx:1.26-alpine \
  sh -c 'echo kept-through-it-all >/data/note; echo e2e-take-on-says-hello; exec nginx -g "daemon off;"' >/dev/null ||
  fail "docker run e2e-take-on failed"
take_on_started=$(touched e2e-take-on) || fail "docker inspect e2e-take-on failed"
# A plan shows what the list of containers leaves out, so a token can't even plan one.
if said=$(dw containers adopt e2e-take-on --service taken --plan-only 2>&1); then fail "a token planned taking a container on: $said"; fi
take_on e2e-take-on taken
adopt_change=$(jq -r '.change.id' <<<"$taken_plan") || fail "the plan isn't JSON: $taken_plan"
[[ "$(docker inspect --format '{{.Name}}' e2e-take-on)" == /e2e-take-on ]] || fail "taking it on renamed it"
# Undoing that lets go of a live container, and that moves nothing either.
dw changes undo "$adopt_change" --yes || fail "undoing taking it on failed"
[[ "$(touched e2e-take-on)" == "$take_on_started" ]] ||
  fail "letting it go touched e2e-take-on: $take_on_started, then $(touched e2e-take-on)"
listed=$(dw containers list --json) || fail "containers list failed"
[[ "$listed" == *'"e2e-take-on"'* ]] || fail "let go of, e2e-take-on isn't among what Dockwell didn't make: $listed"
take_on e2e-take-on taken
listed=$(dw containers list --json) || fail "containers list failed"
[[ "$listed" != *'"e2e-take-on"'* ]] || fail "e2e-take-on is still among what Dockwell didn't make: $listed"
said=$(dw logs read taken) || fail "logs read taken failed"
[[ "$said" == *e2e-take-on-says-hello* ]] || fail "the service's log doesn't have what the container printed: $said"
drift=$(dw drift list --json) || fail "drift list failed"
[[ "$(jq '.drift // [] | length' <<<"$drift")" == 0 ]] || fail "what was taken on reads as drift: $drift"
history=$(dw changes list --json) || fail "changes list failed"
[[ "$history" != *not-in-any-plan* ]] || fail "the change's record shows a value of the environment: $history"

# The point of it all: a new tag is an ordinary plan, and what it had comes through.
dw services deploy taken --image nginx:1.27-alpine --yes || fail "deploying a new image of taken failed"
if docker inspect e2e-take-on >/dev/null 2>&1; then fail "the container taken on is still there after a change of image"; fi
image_change=$(dw changes list --json | jq -r '.changes[0].id') || fail "changes list failed"
taken_container() {
  docker ps --filter "label=com.docker.compose.service=taken" --format '{{.Names}}' | head -1
}
holds() {
  local want_image=$1 name
  name=$(taken_container)
  [[ -n "$name" ]] || fail "nothing runs as taken: $(docker ps -a --format '{{.Names}} {{.Status}}')"
  [[ "$(docker inspect --format '{{.Config.Image}}' "$name")" == "$want_image" ]] ||
    fail "taken runs $(docker inspect --format '{{.Config.Image}}' "$name"), want $want_image"
  docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$name" | grep -qx 'E2E_TAKEN=not-in-any-plan' ||
    fail "taken lost its environment"
  [[ "$(docker inspect --format '{{.HostConfig.RestartPolicy.Name}}' "$name")" == always ]] || fail "taken lost its restart policy"
  [[ "$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$name")" == e2e-take-on-data ]] ||
    fail "taken doesn't mount the volume it came with: $(docker inspect --format '{{json .Mounts}}' "$name")"
  [[ "$(docker exec "$name" cat /data/note)" == kept-through-it-all ]] || fail "what taken wrote to its volume is gone"
  docker inspect --format '{{range $n, $e := .NetworkSettings.Networks}}{{println $n}}{{end}}' "$name" | grep -qx e2e-by-hand-net ||
    fail "taken left the network it was on"
  docker inspect --format '{{json .NetworkSettings.Networks}}' "$name" | grep -q '"e2e-take-on"' ||
    fail "taken no longer answers to the name it came with: $(docker inspect --format '{{json .NetworkSettings.Networks}}' "$name")"
  for _ in $(seq 100); do
    curl -fsS -o /dev/null http://127.0.0.1:18096/ && return 0
    sleep 0.1
  done
  fail "taken doesn't answer on its port"
}
holds nginx:1.27-alpine
dw changes undo "$image_change" --yes || fail "undoing the change of image failed"
holds nginx:1.26-alpine
dw services delete taken --yes || fail "deleting taken failed"
docker network rm e2e-by-hand-net >/dev/null || fail "docker network rm e2e-by-hand-net failed"
docker volume rm e2e-take-on-data >/dev/null || fail "docker volume rm e2e-take-on-data failed"

step "What docker compose made, and what runs on Docker's default bridge, are taken on, and each takes a new image"
real_projects+=(e2e-composed)
composed_file=$(mktemp)
cat >"$composed_file" <<'EOF_COMPOSE'
services:
  app:
    image: nginx:1.26-alpine
    ports: ["127.0.0.1:18098:80"]
    expose: ["3000"]
    volumes: ["composed-data:/data"]
    environment:
      E2E_COMPOSED: not-in-any-plan
    command: ["sh", "-c", "echo kept-from-compose >/data/note; exec nginx -g 'daemon off;'"]
volumes:
  composed-data: {}
EOF_COMPOSE
compose_e2e() { docker compose -p e2e-composed -f "$composed_file" "$@"; }
compose_e2e up --detach --wait >/dev/null || fail "docker compose up failed"
take_on e2e-composed-app-1 composed
jq -e '.change.plan.notes | any(.reason == "managed_elsewhere" and (.message | test("Compose project e2e-composed")))' <<<"$taken_plan" >/dev/null ||
  fail "the plan doesn't say something may still run what it takes on: $taken_plan"
# What that note tells a person has to be what happens: until the image changes,
# what runs the project acts on this same container.
composed_id() { docker inspect --format '{{.Id}}' e2e-composed-app-1; }
composed_started() { docker inspect --format '{{.State.StartedAt}}' e2e-composed-app-1; }
was_id=$(composed_id) || fail "docker inspect e2e-composed-app-1 failed"
was_started=$(composed_started) || fail "docker inspect e2e-composed-app-1 failed"
compose_e2e restart >/dev/null 2>&1 || fail "docker compose restart failed"
[[ "$(composed_id)" == "$was_id" && "$(composed_started)" != "$was_started" ]] ||
  fail "docker compose restart didn't act on what was taken on: $was_id at $was_started, then $(composed_id) at $(composed_started)"
# And the first plan that replaces it says so again.
replacing=$(dw services deploy composed --image nginx:1.27-alpine --plan-only --json) || fail "planning a new image of composed failed: $replacing"
jq -e '.change.plan.notes | any(.reason == "managed_elsewhere")' <<<"$replacing" >/dev/null ||
  fail "the first plan that replaces it doesn't say what else may run it: $replacing"
dw changes discard "$(jq -r '.change.id' <<<"$replacing")" >/dev/null || fail "changes discard failed"
docker run --detach --name e2e-on-bridge --publish 127.0.0.1:18099:80 nginx:1.26-alpine >/dev/null || fail "docker run e2e-on-bridge failed"
# What shares its network would have only loopback once it's replaced, so it's refused
# by name, on real Docker's way of saying that.
docker run --detach --name e2e-sidecar --network container:e2e-on-bridge nginx:1.26-alpine sleep 600 >/dev/null || fail "docker run e2e-sidecar failed"
leaned=$(person_answer ContainerService/AdoptContainer '{"id": "e2e-on-bridge", "service": "on-bridge"}') || fail "a person can't reach the API"
[[ "$leaned" == *"e2e-sidecar shares its network"* ]] || fail "taking on what another shares the network of wasn't refused by its name: $leaned"
docker rm --force e2e-sidecar >/dev/null || fail "docker rm e2e-sidecar failed"
take_on e2e-on-bridge on-bridge
# new_image_serves deploys a new image to a service and checks its port answers.
new_image_serves() {
  dw services deploy "$1" --image nginx:1.27-alpine --yes || fail "deploying a new image of $1 failed"
  running=$(docker ps --filter "label=com.docker.compose.service=$3" --format '{{.Names}} {{.Image}}')
  [[ "$running" == *nginx:1.27-alpine* ]] || fail "$1 runs: $running"
  for _ in $(seq 100); do
    curl -fsS -o /dev/null "http://127.0.0.1:$2/" && return 0
    sleep 0.1
  done
  fail "$1 doesn't answer on port $2 after its new image: $(docker ps -a --format '{{.Names}} {{.Status}}')"
}
new_image_serves composed 18098 composed
new_image_serves on-bridge 18099 on-bridge
composed=$(docker ps --filter label=com.docker.compose.service=composed --format '{{.Names}}' | head -1)
[[ "$(docker exec "$composed" cat /data/note)" == kept-from-compose ]] || fail "composed lost what it wrote"
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$composed" | grep -qx 'E2E_COMPOSED=not-in-any-plan' || fail "composed lost its environment"
docker inspect --format '{{json .Config.ExposedPorts}}' "$composed" | grep -q '"3000/tcp"' ||
  fail "composed lost the port it exposes: $(docker inspect --format '{{json .Config.ExposedPorts}}' "$composed")"
# After the change of image, the same project makes a second copy on the same
# volume, as the note says. Its port is the service's, so it may not start.
compose_e2e up --detach >/dev/null 2>&1 || true
second=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' e2e-composed-app-1) ||
  fail "docker compose up made no second copy of what was replaced, which the plan says it does"
[[ "$second" == e2e-composed_composed-data ]] || fail "the second copy mounts $second, not what the service does"
docker rm --force e2e-composed-app-1 >/dev/null || fail "docker rm e2e-composed-app-1 failed"
rm -f "$composed_file"
if docker inspect e2e-on-bridge >/dev/null 2>&1; then fail "what was on the default bridge is still there after a change of image"; fi
dw services delete composed --yes || fail "deleting composed failed"
dw services delete on-bridge --yes || fail "deleting on-bridge failed"
docker compose -p e2e-composed down --volumes --timeout 1 >/dev/null || fail "docker compose down for e2e-composed failed"

step "A Compose project made elsewhere is taken on whole, with no restart, and one of its images changes"
real_projects+=(e2e-taken)
docker compose -p e2e-taken -f - up --detach --wait >/dev/null <<'EOF_COMPOSE' || fail "docker compose up failed"
services:
  front:
    image: nginx:1.26-alpine
    ports: ["127.0.0.1:18097:80"]
    environment:
      FRONT_KEY: not-in-any-plan
  store:
    image: nginx:1.26-alpine
    volumes: ["kept:/data"]
    command: ["sh", "-c", "echo kept-with-the-project >/data/note; exec nginx -g 'daemon off;'"]
volumes:
  kept: {}
EOF_COMPOSE
compose_state() { docker inspect --format '{{.Id}} {{.RestartCount}} {{.State.StartedAt}}' e2e-taken-front-1 e2e-taken-store-1; }
compose_before=$(compose_state) || fail "docker inspect of the project's containers failed"
# As for one container, a token can't even plan it.
if said=$(dw compose adopt --name e2e-taken --plan-only 2>&1); then fail "a token planned taking a project on: $said"; fi
adopted=$(person_answer ComposeService/AdoptCompose '{"name": "e2e-taken"}') || fail "a person can't reach the API to take on e2e-taken"
jq -e '.change.id' <<<"$adopted" >/dev/null || fail "taking on e2e-taken was refused: $adopted"
[[ "$adopted" != *not-in-any-plan* ]] || fail "the plan shows a value of an environment: $adopted"
jq -e '.change.plan.notes | any(.reason == "managed_elsewhere")' <<<"$adopted" >/dev/null ||
  fail "the plan doesn't say something may still run the project: $adopted"
person_apply "$(jq -r '.change.id' <<<"$adopted")"
[[ "$(compose_state)" == "$compose_before" ]] || fail "taking the project on touched it: $compose_before, then $(compose_state)"
docker compose -p e2e-taken ps --format '{{.Name}}' | grep -qx e2e-taken-front-1 ||
  fail "docker compose no longer lists what it made: $(docker compose -p e2e-taken ps)"
listed=$(dw containers list --json) || fail "containers list failed"
[[ "$listed" != *e2e-taken-* ]] || fail "the project's containers are still among what Dockwell didn't make: $listed"
dw services deploy front --project e2e-taken --image nginx:1.27-alpine --yes || fail "deploying a new image of front failed"
# Dockwell's container is in its environment's Compose project, env_…, whatever it's
# named (ADR 0035), and the one Compose made is in e2e-taken.
front=$(docker ps --filter label=com.docker.compose.service=front --format '{{.Label "com.docker.compose.project"}} {{.Names}}' | awk '$1 ~ /^env_/ { print $2; exit }')
[[ -n "$front" ]] || fail "nothing of Dockwell's runs as front: $(docker ps --format '{{.Names}} {{.Image}}')"
[[ "$(docker inspect --format '{{.Config.Image}}' "$front")" == nginx:1.27-alpine ]] || fail "front runs $(docker inspect --format '{{.Config.Image}}' "$front")"
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$front" | grep -qx 'FRONT_KEY=not-in-any-plan' || fail "front lost its environment"
# It reaches the other service by name, on the network the project came with.
docker exec "$front" sh -c 'wget -q -O /dev/null http://store/' || fail "front can't reach store by name"
[[ "$(docker inspect --format '{{.State.StartedAt}}' e2e-taken-store-1)" == "$(awk '/./ { s = $3 } END { print s }' <<<"$compose_before")" ]] ||
  fail "changing front's image touched store"
[[ "$(docker exec e2e-taken-store-1 cat /data/note)" == kept-with-the-project ]] || fail "store lost what it wrote"
dw services delete front --project e2e-taken --yes || fail "deleting front failed"
dw services delete store --project e2e-taken --yes || fail "letting store go failed"
docker inspect e2e-taken-store-1 >/dev/null 2>&1 || fail "deleting a service that was only taken on removed its container"
# The project stays once its services go, which leaves every bare command below a
# choice of two. It and its environment go in one change, as neither can go alone.
dw changes propose --summary "Retire e2e-taken" \
  --edits '{"resource":{"kind":"environment","scope":"e2e-taken","name":"production"},"delete":true}' \
  --edits '{"resource":{"kind":"project","name":"e2e-taken"},"delete":true}' --yes >/dev/null ||
  fail "deleting the project e2e-taken and its environment failed"
docker compose -p e2e-taken down --volumes --timeout 1 >/dev/null || fail "docker compose down for e2e-taken failed"

step "It changes, and the change undoes"
dw services update web --app-image nginx:1.27-alpine --app-ports "$ports" --app-env GREETING=hi --yes ||
  fail "services update failed"
says '^GREETING=hi$' env_of || fail "after the update, web's environment is $(env_of)"
# What the update replaced is gone from Docker, and its log still reads.
sources=$(dw logs sources web --json) || fail "logs sources failed"
kept=$(jq -r '[.sources[] | select(.state == "LOG_SOURCE_STATE_KEPT")] | first | .revision // empty' <<<"$sources")
[[ -n "$kept" ]] || fail "the replaced container's log wasn't kept: $sources"
kept_log=$(dw logs read web --revision "$kept") || fail "logs read --revision $kept failed"
grep -q 'e2e-follows-this' <<<"$kept_log" || fail "the replaced container's kept log doesn't read back: $kept_log"
# Changes are listed newest first.
changes=$(dw changes list --json) || fail "changes list failed"
change=$(jq -r '.changes[0].id // empty' <<<"$changes") || fail "changes list printed what isn't JSON: $changes"
[[ "$change" == chg_* ]] || fail "changes list has no change: $changes"
dw changes undo "$change" --yes || fail "changes undo $change failed"
says '^GREETING=hello$' env_of || fail "after the undo, web's environment is $(env_of)"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after the undo: $drift"

step "The environment exports as Compose, and docker compose lists it"
dw compose export >"$data/compose.yaml" || fail "compose export failed"
grep -q "^name: $project" "$data/compose.yaml" || fail "the export isn't project $project:
$(cat "$data/compose.yaml")"
if grep -q hello "$data/compose.yaml"; then fail "the export holds an environment variable's value"; fi
GREETING=hello docker compose -f "$data/compose.yaml" config --quiet || fail "docker compose doesn't take the export"
listed=$(GREETING=hello docker compose -f "$data/compose.yaml" ps --format '{{.Service}}') || fail "docker compose -f <export> ps failed"
[[ "$listed" == web ]] || fail "docker compose -f <export> ps lists '$listed', want web"
listed=$(docker compose -p "$project" ps --format '{{.Service}}') || fail "docker compose -p $project ps failed"
[[ "$listed" == web ]] || fail "docker compose -p $project ps lists '$listed', want web"

step "A Compose file imports"
cat >"$data/stack.yaml" <<'EOF'
services:
  cache:
    image: redis:7.4-alpine
    volumes: [cache-data:/data]
    networks: [back]
volumes:
  cache-data: {}
networks:
  back: {}
EOF
# A file made to cost too much is stopped by the limited child that reads it
# first, and the control plane carries on. One long word through a variable
# goes through a quadratic splitter, for far longer than a child is allowed.
long=$(head -c 120000 /dev/zero | tr '\0' x)
# shellcheck disable=SC2016 # ${A} is Compose's, for dockwell to read.
costly_file=$(printf 'services:\n  slow:\n    image: nginx:1.27-alpine\n    command: ${A}${A}${A}${A}\n')
if costly=$(dw compose import --compose "$costly_file" --variables "A=$long" --yes 2>&1); then
  fail "a file made to cost too much was read: ${costly:0:300}"
fi
[[ "$costly" == *"more time or memory"* ]] || fail "a file made to cost too much failed some other way: ${costly:0:300}"
dw changes list --json >/dev/null || fail "the control plane didn't carry on after a file that cost too much"

dw compose import --project default --compose "$(cat "$data/stack.yaml")" --yes || fail "compose import failed"
services=$(docker compose -p "$project" ps --format '{{.Service}}' | sort | tr '\n' ' ') || fail "docker compose -p $project ps failed"
[[ "$services" == "cache web " ]] || fail "after the import, docker compose lists '$services', want cache and web"
docker volume inspect "${project}_cache-data" >/dev/null || fail "the import made no volume ${project}_cache-data"

step "A service using a variable and a secret runs with their values"
secret_value=e2e-s3cret-value
# The secret's value comes from a file: the command line would put it in the
# process list, and the CLI takes it no other way.
printf '%s' "$secret_value" >"$data/secret-value"
dw variables create LOG_LEVEL --value debug --yes || fail "variables create failed"
dw secrets create DB_PASSWORD --value-file "$data/secret-value" --yes || fail "secrets create failed"
dw services create api --app-image nginx:1.27-alpine --variables LOG_LEVEL= --secrets DATABASE_PASSWORD=DB_PASSWORD --yes ||
  fail "services create api failed"
api=$(docker ps --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=api --format '{{.Names}}') ||
  fail "docker ps failed"
[[ "$api" == "$readable.api-1" ]] || fail "Docker runs '$api' for api, want $readable.api-1"
api_env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$api") || fail "docker inspect $api failed"
grep -qx "DATABASE_PASSWORD=$secret_value" <<<"$api_env" || fail "api doesn't run with the secret's value"
grep -qx LOG_LEVEL=debug <<<"$api_env" || fail "api doesn't run with the variable's value"
api_id=$(docker inspect --format '{{.Id}}' "$api") || fail "docker inspect $api failed"
api_labels=$(docker inspect --format '{{json .Config.Labels}}' "$api") || fail "docker inspect $api failed"
if [[ "$api_labels" == *"$secret_value"* ]]; then fail "api's labels hold the secret's value"; fi
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift with api running: $drift"

step "The environment exports without the secret, and docker compose lists it"
dw compose export >"$data/values.yaml" || fail "compose export failed"
if grep -q -e "$secret_value" "$data/values.yaml"; then fail "the export holds the secret's value"; fi
grep -Eq '^ +DATABASE_PASSWORD: null$' "$data/values.yaml" || fail "the export doesn't name DATABASE_PASSWORD without a value:
$(cat "$data/values.yaml")"
grep -Eq '^ +LOG_LEVEL: debug$' "$data/values.yaml" || fail "the export doesn't write LOG_LEVEL's value:
$(cat "$data/values.yaml")"
docker compose -f "$data/values.yaml" config --quiet || fail "docker compose doesn't take the export"
services=$(docker compose -f "$data/values.yaml" ps --format '{{.Service}}' | sort | tr '\n' ' ') || fail "docker compose -f <export> ps failed"
[[ "$services" == "api cache web " ]] || fail "docker compose -f <export> ps lists '$services', want api, cache and web"

step "Importing it again plans no change, and restarts nothing"
# web's own environment is by name in the file, so it comes as a variable, as
# any value of a container's own does. api's comes from what it uses. With
# --remove-orphans, anything the import lost would show as going. The CLI
# discards a plan with nothing in it, and says so on standard error.
imported=$(dw compose import --compose "$(cat "$data/values.yaml")" --variables GREETING=hello --remove-orphans --json 2>"$data/import.err") ||
  fail "compose import of the export failed: $(cat "$data/import.err")"
grep -q 'Nothing to change' "$data/import.err" || fail "compose import of the export didn't say there was nothing to change: $(cat "$data/import.err")"
# Each check says something an empty answer can't pass: a file that imported
# nothing has no api to find, and no plan has no summary to add up.
jq -e '.change.id | startswith("chg_")' <<<"$imported" >/dev/null || fail "compose import printed no change: $imported"
jq -e '[.change.plan.resources[] | select(.resource.kind == "service") | .resource.name] | sort == ["api", "cache", "web"]' <<<"$imported" >/dev/null ||
  fail "the import didn't plan api, cache and web: $imported"
jq -e '.change.plan | has("fingerprint")' <<<"$imported" >/dev/null || fail "the import has no plan: $imported"
jq -e '.change.plan.summary // {} | [.create, .change, .replace, .destroy] | map(. // 0) | add == 0' <<<"$imported" >/dev/null ||
  fail "importing the export plans a change: $imported"
jq -e '[.change.plan.resources[] | select((.changes // []) | length > 0)] | length == 0' <<<"$imported" >/dev/null ||
  fail "importing the export changes a field: $imported"
jq -e '[.change.plan.servers[]?.steps[]? | select((.action // "") | IN("", "PLAN_ACTION_NONE") | not)] | length == 0' <<<"$imported" >/dev/null ||
  fail "importing the export runs a step: $imported"
if [[ "$imported" == *"$secret_value"* ]]; then fail "the plan holds the secret's value"; fi

step "A container docker compose makes from the export reads back as the service"
DATABASE_PASSWORD=$secret_value docker compose -f "$data/values.yaml" up --detach --no-deps api || fail "docker compose up api failed"
# Compose names what it makes itself, and Dockwell takes it as the service's by
# its labels, under that name, and puts no name of its own back (ADR 0035).
api=$(named_container api)
[[ "$api" == "$project-api-1" ]] || fail "docker compose up made '$api' for api, want $project-api-1, as Compose names it"
[[ "$(docker inspect --format '{{.Id}}' "$api")" != "$api_id" ]] || fail "docker compose up didn't recreate api, so this reads back nothing new"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after docker compose up from the export: $drift"
# The agent reads what it prints as the service's, by its name, as a person reads
# it in docker ps.
sources=$(dw logs sources api --json) || fail "logs sources api failed"
jq -e --arg c "$api" 'any(.sources[]; .containerName == $c and .state == "LOG_SOURCE_STATE_RUNNING")' <<<"$sources" >/dev/null ||
  fail "the agent doesn't read $api as api's: $sources"

step "The same with no value in the shell reads as drift, and with it back, as none"
env -u DATABASE_PASSWORD docker compose -f "$data/values.yaml" up --detach --no-deps --force-recreate api || fail "docker compose up api failed"
api=$(named_container api)
api_env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$api") || fail "docker inspect $api failed"
if grep -q '^DATABASE_PASSWORD=' <<<"$api_env"; then fail "docker compose gave api the secret from nowhere, so this reads back nothing new"; fi
drift=$(dw drift list --json) || fail "drift list failed"
jq -e 'any((.drift // [])[]; .resource.name == "api")' <<<"$drift" >/dev/null || fail "api without its secret isn't drift: $drift"
if [[ "$drift" == *"$secret_value"* ]]; then fail "drift holds the secret's value"; fi
DATABASE_PASSWORD=$secret_value docker compose -f "$data/values.yaml" up --detach --no-deps --force-recreate api || fail "docker compose up api failed"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift with the secret back: $drift"

step "A name with no value takes an image's own value out of what runs, which reads as drift"
# NGINX_VERSION is in the image and in no spec. Docker keeps the name in Config.Env, and
# what runs has neither it nor the image's value, so it differs from what the spec says.
sed -E 's/^( +)DATABASE_PASSWORD: null$/&\n\1NGINX_VERSION: null/' "$data/values.yaml" >"$data/bare-name.yaml" || fail "editing the export failed"
grep -Eq '^ +NGINX_VERSION: null$' "$data/bare-name.yaml" || fail "the edited export doesn't name NGINX_VERSION:
$(cat "$data/bare-name.yaml")"
env -u NGINX_VERSION DATABASE_PASSWORD=$secret_value docker compose -f "$data/bare-name.yaml" up --detach --no-deps --force-recreate api || fail "docker compose up api failed"
api=$(named_container api)
api_env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$api") || fail "docker inspect $api failed"
grep -qx NGINX_VERSION <<<"$api_env" || fail "docker compose didn't send NGINX_VERSION by name alone, so this reads back nothing new: $api_env"
running=$(docker exec "$api" env) || fail "docker exec $api env failed"
if grep -q '^NGINX_VERSION=' <<<"$running"; then fail "what runs still has the image's NGINX_VERSION, so a name with no value doesn't take it out, as ADR 0021 says"; fi
drift=$(dw drift list --json) || fail "drift list failed"
jq -e 'any((.drift // [])[]; .resource.name == "api")' <<<"$drift" >/dev/null || fail "api without the image's value isn't drift: $drift"
DATABASE_PASSWORD=$secret_value docker compose -f "$data/values.yaml" up --detach --no-deps --force-recreate api || fail "docker compose up api failed"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift with the image's value back: $drift"

step "A rotation reaches what docker compose made from the export"
secret_next=e2e-s3cret-next
printf '%s' "$secret_next" >"$data/secret-value"
dw secrets update DB_PASSWORD --value-file "$data/secret-value" --yes || fail "secrets update failed"
# It rolls as any change does, to the next number.
api=$(docker ps -a --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=api --format '{{.Names}}') ||
  fail "docker ps failed"
[[ "$api" == "$readable.api-2" ]] || fail "after the rotation, Docker has '$api' for api, want only $readable.api-2: it rolls to the next number, from the $project-api-1 docker compose made"
api_env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$api") || fail "docker inspect $api failed"
grep -qx "DATABASE_PASSWORD=$secret_next" <<<"$api_env" || fail "the rotation didn't reach api"

dw services delete api --yes || fail "services delete api failed"
dw secrets delete DB_PASSWORD --yes || fail "secrets delete failed"
dw variables delete LOG_LEVEL --yes || fail "variables delete failed"
# Nothing above left a value where anyone reads it.
if grep -q -e "$secret_value" -e "$secret_next" "$log"; then fail "the control plane's log holds the secret's value"; fi

step "A password in a command runs and works, and nothing that reads it shows it"
redis_password=e2e-redis-pass-7Hq2mXv9
redis_given=e2e-redis-given-5Lt1oGy7
redis_secret=e2e-redis-secret-9Rk4eSa8
redis_rotated=e2e-redis-rotated-2Dp8iJr6
# Of the shape 1Panel's store gives Redis, with a $ for the check's own shell beside the name.
cat >"$data/redis.yaml" <<'EOF'
services:
  secured:
    image: redis:7.4-alpine
    command: redis-server ${REDIS_PASSWORD:+--requirepass "${REDIS_PASSWORD}"}
    healthcheck:
      test: ["CMD-SHELL", "redis-cli -a \"${REDIS_PASSWORD}\" --no-auth-warning -p $${REDIS_CHECK_PORT:-6379} ping | grep -q PONG"]
      interval: 1s
      timeout: 3s
      retries: 30
EOF
printf 'REDIS_PASSWORD=%s\n' "$redis_password" >"$data/redis.env"
# answers says whether a container's Redis takes a password: the real proof that
# what the command names reached what runs, and that what it no longer names doesn't.
answers() { [[ "$(docker exec "$1" redis-cli -a "$2" --no-auth-warning ping 2>&1)" == PONG ]]; }
# nothing_shows reads all the CLI can of the install, its logs and the database's own
# files, and fails on any of the values. Docker has them: that is the point, and root
# on this server reads them there.
nothing_shows() {
  local reads value
  reads=$({
    dw changes list --page-size 200 --json
    dw services list --json
    dw compose export
    dw drift list --json
    dw events list --page-size 200 --json
  }) || fail "reading what the install says failed"
  for id in $(dw changes list --page-size 200 --json | jq -r '.changes[].id'); do
    reads+=$(dw changes get "$id" --json) || fail "changes get $id failed"
  done
  for value in "$@"; do
    if [[ "$reads" == *"$value"* ]]; then fail "what the install says holds $value: $(grep -F -- "$value" <<<"$reads" | head -3)"; fi
    if grep -q -F -- "$value" "$log"; then fail "the control plane's log holds $value"; fi
    # As whoever runs Dockwell, whose directory this shell may not be able to list, and
    # only 1 says it read them and didn't find it: anything else would pass for that.
    found=0
    # shellcheck disable=SC2016 # for the shell it starts to read
    priv sh -c 'grep -a -q -F -- "$1" "$2"/control/state.db*' sh "$value" "$data" || found=$?
    case $found in
      0) fail "the database's own files hold $value" ;;
      1) ;;
      *) fail "can't read the database's own files, so nothing says they don't hold $value" ;;
    esac
  done
}
# delete_kept deletes a service whose image keeps data, as redis:7.4-alpine does at
# /data, so a person applies it, and no token can. The server keeps that data for 7
# days, for an undo. This step leaves nothing behind, so it removes it, which the
# agent's prune takes as already done.
delete_kept() {
  local planned container volumes
  container=$(named_container "$1")
  volumes=$(docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{println .Name}}{{end}}{{end}}' "$container") ||
    fail "docker inspect $container failed"
  [[ -n "$volumes" ]] || fail "$container has no volume for what its image keeps, so this deletes nothing a person must approve"
  planned=$(dw services delete "$1" --plan-only --json) || fail "planning to delete $1 failed"
  person_apply "$(jq -r '.change.id' <<<"$planned")"
  # shellcheck disable=SC2086 # one name to a line
  docker volume rm $volumes >/dev/null || fail "can't remove what $1's image kept: $volumes"
}
# found is what the server has, to hold this step to leaving it as it found it: the
# import makes the environment's default network, and each service makes a volume for
# what its image keeps.
found() {
  local containers networks volumes drift
  containers=$(docker ps -a --format '{{.Names}}' | sort | tr '\n' ' ') || return 1
  networks=$(docker network ls --format '{{.Name}}' | sort | tr '\n' ' ') || return 1
  volumes=$(docker volume ls -q | sort | tr '\n' ' ') || return 1
  drift=$(dw drift list --json | jq -c '[(.drift // [])[] | .resource] | sort_by(.kind, .name)') || return 1
  echo "containers: $containers; networks: $networks; volumes: $volumes; drift: $drift"
}
found_before=$(found) || fail "can't read what the server has"
dw compose import --project default --compose "$(cat "$data/redis.yaml")" --env-file "$data/redis.env" --yes ||
  fail "importing a password in a command failed"
secured=$(named_container secured)
secured_id=$(docker inspect --format '{{.Id}}' "$secured") || fail "docker inspect $secured failed"
answers "$secured" "$redis_password" || fail "$secured doesn't take the password it was given"
if answers "$secured" not-the-password; then fail "$secured takes any password, so no password reached it"; fi
[[ "$(docker inspect --format '{{.State.Health.Status}}' "$secured")" == healthy ]] || fail "$secured isn't healthy: its check's name, or the \$ beside it, didn't reach its shell"
docker inspect --format '{{json .Config.Cmd}}' "$secured" | grep -q -F -- "$redis_password" ||
  fail "$secured doesn't run with the password in its command: $(docker inspect --format '{{json .Config.Cmd}}' "$secured")"
if docker inspect --format '{{json .Config.Labels}}' "$secured" | grep -q -F -- "$redis_password"; then fail "$secured's labels hold the password"; fi
nothing_shows "$redis_password"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift with a password in a command: $drift"

step "The same file with the same value changes nothing"
imported=$(dw compose import --project default --compose "$(cat "$data/redis.yaml")" --env-file "$data/redis.env" --json 2>"$data/import.err") ||
  fail "importing it again failed: $(cat "$data/import.err")"
grep -q 'Nothing to change' "$data/import.err" || fail "importing it again didn't say there was nothing to change: $(cat "$data/import.err")"
if [[ "$imported" == *"$redis_password"* ]]; then fail "the plan holds the password"; fi
[[ "$(docker inspect --format '{{.Id}}' "$secured")" == "$secured_id" ]] || fail "importing it again recreated $secured"

step "An export names it, and what docker compose makes of that reads back the same"
dw compose export >"$data/redis-export.yaml" || fail "compose export failed"
if grep -q -F -- "$redis_password" "$data/redis-export.yaml"; then fail "the export holds the password"; fi
# shellcheck disable=SC2016 # Compose's, for docker compose to read.
grep -q -F -- '${REDIS_PASSWORD}' "$data/redis-export.yaml" || fail "the export doesn't name it: $(cat "$data/redis-export.yaml")"
grep -Eq '^ +REDIS_PASSWORD: null$' "$data/redis-export.yaml" || fail "the export doesn't list it to set: $(cat "$data/redis-export.yaml")"
# shellcheck disable=SC2016 # Compose's, for docker compose to read.
grep -q -F -- '$${REDIS_CHECK_PORT:-6379}' "$data/redis-export.yaml" || fail "the export lost the check's own \$: $(cat "$data/redis-export.yaml")"
REDIS_PASSWORD=$redis_password docker compose -f "$data/redis-export.yaml" up --detach --no-deps --wait secured || fail "docker compose up secured failed"
secured=$(named_container secured)
[[ "$(docker inspect --format '{{.Id}}' "$secured")" != "$secured_id" ]] || fail "docker compose up didn't recreate $secured, so this reads back nothing new"
answers "$secured" "$redis_password" || fail "what docker compose made doesn't take the password"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after docker compose up from the export: $drift"

step "A new value is a change that reaches what runs, and says so without it"
printf 'REDIS_PASSWORD=%s\n' "$redis_given" >"$data/redis.env"
rotated=$(dw compose import --project default --compose "$(cat "$data/redis.yaml")" --env-file "$data/redis.env" --plan-only --json) || fail "planning a new value failed"
jq -e '[.change.plan.resources[].changes[]? | select(.path == "app.env[REDIS_PASSWORD]" and .sensitive == true)] | length == 1' <<<"$rotated" >/dev/null ||
  fail "a new value doesn't plan as a masked change: $rotated"
if [[ "$rotated" == *"$redis_given"* || "$rotated" == *"$redis_password"* ]]; then fail "the plan holds a value"; fi
rotated_id=$(jq -r '.change.id' <<<"$rotated")
dw changes apply "$rotated_id" --yes || fail "applying a new value failed"
secured=$(named_container secured)
answers "$secured" "$redis_given" || fail "$secured doesn't take the new password"
if answers "$secured" "$redis_password"; then fail "$secured still takes the old password"; fi
nothing_shows "$redis_password" "$redis_given"
delete_kept secured
# The import made the environment's default network for it, which nothing else is on.
# Left, it would go in the ingress step's docker network prune, and read as drift.
dw networks delete default --yes || fail "networks delete default failed"

step "A secret in a command needs no value in a file, can't be given one, and rotates"
printf '%s' "$redis_secret" >"$data/secret-value"
dw secrets create REDIS_SECRET --value-file "$data/secret-value" --yes || fail "secrets create failed"
# shellcheck disable=SC2016 # ${PW} is for Dockwell to read.
dw services create named --app-image redis:7.4-alpine --app-command redis-server --app-command=--requirepass --app-command '${PW}' --app-references command --secrets PW=REDIS_SECRET --yes ||
  fail "services create named failed"
named=$(named_container named)
answers "$named" "$redis_secret" || fail "$named doesn't take the secret's value"
if answers "$named" not-the-password; then fail "$named takes any password"; fi
dw compose export >"$data/named.yaml" || fail "compose export failed"
if grep -q -F -- "$redis_secret" "$data/named.yaml"; then fail "the export holds the secret's value"; fi
# web's own environment is by name in the file, so it comes as a variable. named's
# comes from what it uses, so nothing is asked for it.
imported=$(dw compose import --project default --compose "$(cat "$data/named.yaml")" --variables GREETING=hello --json 2>"$data/import.err") || fail "importing the export failed: $(cat "$data/import.err")"
grep -q 'Nothing to change' "$data/import.err" || fail "importing the export didn't say there was nothing to change: $(cat "$data/import.err")"
if [[ "$imported" == *"$redis_secret"* ]]; then fail "the plan holds the secret's value"; fi
for guess in "$redis_secret" not-the-password; do
  if said=$(dw compose import --project default --compose "$(cat "$data/named.yaml")" --variables GREETING=hello --variables "PW=$guess" --plan-only 2>&1); then fail "a file gave the secret a value: $said"; fi
  [[ "$said" == *"from a secret too"* ]] || fail "a value for the secret failed some other way: $said"
  if [[ "$said" == *"$redis_secret"* ]]; then fail "the refusal repeats the value: $said"; fi
done
printf '%s' "$redis_rotated" >"$data/secret-value"
dw secrets update REDIS_SECRET --value-file "$data/secret-value" --yes || fail "secrets update failed"
named=$(named_container named)
answers "$named" "$redis_rotated" || fail "the rotation didn't reach $named"
if answers "$named" "$redis_secret"; then fail "$named still takes the old value"; fi
nothing_shows "$redis_secret" "$redis_rotated"
delete_kept named
dw secrets delete REDIS_SECRET --yes || fail "secrets delete failed"
found_after=$(found) || fail "can't read what the server has"
[[ "$found_after" == "$found_before" ]] ||
  fail "the steps with a password in a command left the server other than they found it: before, $found_before; after, $found_after"

step "Services go"
dw services delete cache --yes || fail "services delete cache failed"
dw services delete web --yes || fail "services delete web failed"
left=$(docker compose -p "$project" ps -q) || fail "docker compose -p $project ps failed"
[[ -z "$left" ]] || fail "containers are left after deleting the services: $left"

# Deleting the volume destroys data, so only a person may apply it; the
# CLI says so and leaves it planned, with exit status 4.
set +e
dw volumes delete cache-data --yes >/dev/null 2>&1
status=$?
set -e
[[ "$status" == 4 ]] || fail "volumes delete exited $status, want 4: a plan that destroys data waits for a person"

stop_dockwell() {
  priv kill "$pid"
  wait "$pid" 2>/dev/null || true
  # Everything that says "with Dockwell down" depends on this.
  if curl -fsS --max-time 2 "http://$addr/healthz" >/dev/null 2>&1; then fail "Dockwell still answers after it was stopped"; fi
  connected=$(grep -c 'connected to the control plane' "$log" || true)
}
# wait_for_dockwell waits for what stop_dockwell stopped to answer, and for its
# agent to connect, which every plan needs.
wait_for_dockwell() {
  for _ in $(seq 300); do
    curl -fsS "http://$addr/healthz" >/dev/null 2>&1 &&
      [[ "$(grep -c 'connected to the control plane' "$log")" -gt "$connected" ]] && return 0
    sleep 0.1
  done
  fail "Dockwell didn't come back within 30 s"
}
# kill_dockwell stops it with no warning, in the middle of whatever it's doing, as a
# power cut or the OOM killer would. A clean stop lets a step finish first. As root
# $pid is sudo's, and sudo can't pass on a SIGKILL, so this kills the processes
# themselves, asked for apart from killing, as sudo's own command line would match.
kill_dockwell() {
  local victims
  victims=$(pgrep -f "$bin run --listen $addr") || fail "nothing to kill: Dockwell isn't running"
  # shellcheck disable=SC2086 # one word for each process
  priv kill -KILL $victims || fail "can't kill Dockwell, $victims"
  wait "$pid" 2>/dev/null || true
  if curl -fsS --max-time 2 "http://$addr/healthz" >/dev/null 2>&1; then fail "Dockwell still answers after it was killed"; fi
  connected=$(grep -c 'connected to the control plane' "$log" || true)
}

step "A moving tag: a registry of its own, and two images under one tag"
# The product's first user does this most: a tag that moves, redeployed. Only a
# real engine and a real registry say what a pull really does.
registry_port=""
for p in $(seq 15000 15100); do
  if [[ -z "$(ss -ltnH "sport = :$p")" ]]; then
    registry_port=$p
    break
  fi
done
[[ -n "$registry_port" ]] || fail "no port is free for a registry between 15000 and 15100"
docker run -d --name e2e-registry -p "127.0.0.1:$registry_port:5000" "$registry_image" >/dev/null || fail "can't run a registry"
registry_up() {
  for _ in $(seq 100); do
    curl -fsS "http://127.0.0.1:$registry_port/v2/" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  fail "the registry on port $registry_port didn't answer within 10 s"
}
registry_up
moving_repo=127.0.0.1:$registry_port/e2e-app
moving=$moving_repo:latest
moving_dir=$(mktemp -d)
# build_and_push builds an image that serves its word, pushes it under the one tag,
# and says its digest.
build_and_push() {
  printf '%s\n' "$1" >"$moving_dir/index.html"
  printf 'FROM nginx:1.27-alpine\nCOPY index.html /usr/share/nginx/html/index.html\n' >"$moving_dir/Dockerfile"
  docker build -q -t "e2e-moving:$1" "$moving_dir" >/dev/null || fail "can't build the image $1"
  docker tag "e2e-moving:$1" "$moving" || fail "can't tag the image $1"
  docker push "$moving" | awk '/digest: /{ print $3 }'
}
moving_serves() {
  local got=""
  for _ in $(seq 100); do
    got=$(curl -fsS --max-time 2 "http://127.0.0.1:$web_port/" 2>/dev/null) || got=""
    [[ "$got" == "$1" ]] && return 0
    sleep 0.1
  done
  fail "the app serves '$got' on port $web_port, want '$1'"
}
moving_id() {
  local ids
  ids=$(docker ps -a --no-trunc --filter label=com.docker.compose.service=moving --format '{{.ID}}') || fail "docker ps failed"
  [[ "$(wc -l <<<"$ids")" == 1 && -n "$ids" ]] || fail "Docker has these containers for the app, want one: $ids"
  echo "$ids"
}
moving_started() { docker inspect --format '{{.State.StartedAt}}' "$(moving_id)"; }
moving_revisions() { dw services revisions moving --json; }
image_id_of() { docker image inspect --format '{{.Id}}' "$1"; }
digest_first=$(build_and_push first)
[[ "$digest_first" == sha256:* ]] || fail "pushing the first image said its digest is '$digest_first'"
dw services create moving --app-image "$moving" --app-ports "$ports" --yes || fail "services create moving failed"
moving_serves first

step "A moving tag: an image the registry hasn't got is refused as it plans, by name"
# This registry and not Docker Hub, so that doesn't have to be up for this to pass.
nothing=$(printf 'services:\n  gone:\n    image: %s\n    build: .\n' "$moving_repo:no-such-tag")
if out=$(dw compose import --compose "$nothing" --project default --environment production --plan-only 2>&1); then
  fail "an image no one has was planned: ${out:0:300}"
fi
[[ "$out" == *"won't give it to a pull"* ]] || fail "an image no one has failed some other way: ${out:0:300}"
nothing=$(printf 'services:\n  gone:\n    image: 127.0.0.1:%s/e2e-no-such-repository:1\n    build: .\n' "$registry_port")
if out=$(dw compose import --compose "$nothing" --project default --environment production --plan-only 2>&1); then
  fail "an image of a repository no one has was planned: ${out:0:300}"
fi
[[ "$out" == *"won't give it to a pull"* ]] || fail "an image of a repository no one has failed some other way: ${out:0:300}"

step "A moving tag: deploying it again pulls what it points at now, and replaces what runs"
digest_second=$(build_and_push second)
[[ "$digest_second" == sha256:* && "$digest_second" != "$digest_first" ]] ||
  fail "pushing the second image said its digest is '$digest_second', and the first's is '$digest_first'"
# As on any server, only the registry has the new image: the tag here still says
# the first, so nothing but a real pull can move it.
docker tag e2e-moving:first "$moving" || fail "can't put the tag back on the first image"
docker rmi e2e-moving:second "$moving_repo@$digest_second" >/dev/null || fail "can't remove the second image here"
if docker image inspect "$moving_repo@$digest_second" >/dev/null 2>&1; then fail "the second image is still here"; fi
before=$(moving_id)
planned=$(dw services deploy moving --image "$moving" --plan-only --json 2>/dev/null) || fail "planning the deploy failed"
jq -e --arg want "Pulls $moving again, and replaces moving only if that's a newer image" \
  '.change.plan.notes | any(.reason == "pulls_again" and .message == $want)' <<<"$planned" >/dev/null ||
  fail "the plan doesn't say it pulls, and replaces only if that's newer: $planned"
jq -e '.change.plan.servers[0].steps[0].pull == true' <<<"$planned" >/dev/null || fail "the plan's step doesn't pull: $planned"
change=$(jq -r .change.id <<<"$planned")
read_plan=$(dw changes get "$change") || fail "changes get $change failed"
grep -qF "Note: Pulls $moving again, and replaces moving only if that's a newer image." <<<"$read_plan" ||
  fail "the plan a person reads doesn't say it pulls: $read_plan"
grep -qF "Replace if newer" <<<"$read_plan" || fail "the plan a person reads says it replaces for certain: $read_plan"
# Planning changes nothing on the server.
[[ "$(moving_id)" == "$before" ]] || fail "planning replaced the container"
if docker image inspect "$moving_repo@$digest_second" >/dev/null 2>&1; then fail "planning pulled the image"; fi
dw changes apply "$change" --yes || fail "applying the deploy failed"
moving_did() {
  local did
  did=$(dw changes get "$1" --json | jq -r '.change.steps[0].did') || fail "changes get failed"
  [[ "$did" == "$2" ]] || fail "the step says it did $did, want $2: $(dw changes get "$1" --json)"
}
moving_did "$change" PLAN_ACTION_REPLACE
after=$(moving_id)
[[ "$after" != "$before" ]] || fail "with a newer image, the container wasn't replaced"
if docker inspect "$before" >/dev/null 2>&1; then fail "the old container is still there"; fi
runs=$(docker inspect --format '{{.Image}}' "$after") || fail "docker inspect failed"
[[ "$runs" == "$(image_id_of "$moving_repo@$digest_second")" ]] || fail "the container runs $runs, want the second image"
moving_serves second
revisions=$(moving_revisions) || fail "services revisions failed"
jq -e --arg a "$digest_first" --arg b "$digest_second" --arg image "$moving" \
  '(.revisions | length == 2) and .revisions[0].image == $image and .revisions[0].deployments[0].digest == $b and .revisions[1].deployments[0].digest == $a' \
  <<<"$revisions" >/dev/null || fail "the revisions don't say what ran each time: $revisions"
jq -e '(.revisions[1].matchesLatest // false) == false' <<<"$revisions" >/dev/null || fail "r1 ran another image, and reads as matching the latest: $revisions"

step "A moving tag: with nothing newer, nothing happens, and it says so"
before=$(moving_id)
started=$(moving_started)
planned=$(dw services deploy moving --image "$moving" --plan-only --json 2>/dev/null) || fail "planning the deploy failed"
change=$(jq -r .change.id <<<"$planned")
said=$(dw changes apply "$change" --yes 2>&1) || fail "deploying with nothing newer failed: $said"
moving_did "$change" PLAN_ACTION_NONE
grep -qF "$moving is already the newest image, so moving stays as it is" <<<"$said" || fail "with nothing newer, it says: $said"
if grep -qF "Undo it" <<<"$said"; then fail "it offers to undo what did nothing: $said"; fi
[[ "$(moving_id)" == "$before" ]] || fail "with nothing newer, the container was replaced"
[[ "$(moving_started)" == "$started" ]] || fail "with nothing newer, the container restarted"
revisions=$(moving_revisions) || fail "services revisions failed"
jq -e '.revisions | length == 2' <<<"$revisions" >/dev/null || fail "with nothing newer, a revision was written: $revisions"
moving_serves second

step "A moving tag: a rollback runs what that revision ran, by digest, through the registry"
# Off this server, so only the engine's /distribution can say the registry has it,
# and only a pull by digest can bring it back.
docker rmi e2e-moving:first "$moving_repo@$digest_first" >/dev/null || fail "can't remove the first image here"
if docker image inspect "$moving_repo@$digest_first" >/dev/null 2>&1; then fail "the first image is still here"; fi
planned=$(dw services rollback moving --revision 1 --plan-only --json 2>/dev/null) || fail "planning the rollback failed"
jq -e --arg short "sha256:${digest_first:7:12}" '.change.plan.notes | any(.reason == "digest_pinned" and (.message | contains($short)))' <<<"$planned" >/dev/null ||
  fail "the rollback's plan doesn't say it pins what r1 ran: $planned"
if jq -e '.change.plan.notes | any(.reason == "digest_gone")' <<<"$planned" >/dev/null; then fail "the rollback's plan says the image is gone: $planned"; fi
dw changes apply "$(jq -r .change.id <<<"$planned")" --yes || fail "applying the rollback failed"
runs=$(docker inspect --format '{{.Config.Image}}' "$(moving_id)") || fail "docker inspect failed"
[[ "$runs" == "$moving@$digest_first" ]] || fail "after the rollback Docker has the image as $runs, want $moving@$digest_first"
runs=$(docker inspect --format '{{.Image}}' "$(moving_id)") || fail "docker inspect failed"
[[ "$runs" == "$(image_id_of "$moving_repo@$digest_first")" ]] || fail "after the rollback the container runs $runs, want the first image"
moving_serves first
revisions=$(moving_revisions) || fail "services revisions failed"
jq -e --arg a "$digest_first" '.revisions[0].deployments[0].digest == $a and (.revisions[2].matchesLatest // false)' <<<"$revisions" >/dev/null ||
  fail "after the rollback, the revisions say: $revisions"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after the rollback: $drift"
if refused=$(dw services deploy moving --image "$moving@$digest_first" --plan-only 2>&1); then
  fail "deploying the digest it runs planned: $refused"
fi
grep -qF "already runs $moving@$digest_first" <<<"$refused" || fail "deploying the digest it runs said: $refused"
dw compose export >"$moving_dir/export.yaml" || fail "compose export failed"
grep -qF "image: $moving@$digest_first" "$moving_dir/export.yaml" || fail "the export has $(cat "$moving_dir/export.yaml")"
docker compose -f "$moving_dir/export.yaml" config --quiet || fail "docker compose doesn't take the export of a pinned image"

# moving_settled waits for a failed change's rollback to end. That starts just after
# the failure can be read, so it has to stay quiet for a moment to count.
moving_settled() {
  local quiet=0
  for _ in $(seq 240); do
    if [[ "$(dw changes list --json | jq '[(.changes // [])[] | select(.state == "CHANGE_STATE_APPLYING")] | length')" == 0 ]]; then
      quiet=$((quiet + 1))
      [[ "$quiet" -ge 4 ]] && return 0
    else
      quiet=0
    fi
    sleep 0.5
  done
  fail "a change is still applying after 120 s: $(dw changes list --json)"
}
# cant_pull deploys the tag with the registry stopped: it fails saying the fact,
# what follows and the fix, and what runs goes on as it was.
cant_pull() {
  local id started said
  id=$(moving_id)
  started=$(moving_started)
  if said=$(dw services deploy moving --image "$moving" --yes 2>&1); then fail "deploying with the registry stopped applied: $said"; fi
  grep -qF "can't reach the registry $moving is on" <<<"$said" || fail "with the registry stopped, the fact isn't there: $said"
  grep -qF "Nothing was replaced, and what runs goes on running" <<<"$said" || fail "with the registry stopped, what follows isn't there: $said"
  grep -qF "Check the server's network and DNS" <<<"$said" || fail "with the registry stopped, the fix isn't there: $said"
  moving_settled
  [[ "$(moving_id)" == "$id" ]] || fail "a pull that failed replaced the container"
  [[ "$(moving_started)" == "$started" ]] || fail "a pull that failed restarted the container"
  moving_serves "$1"
}

step "A moving tag: with its registry gone, a pinned app fails before anything stops, and goes on serving"
docker stop e2e-registry >/dev/null || fail "can't stop the registry"
cant_pull first

step "A moving tag: so does one on the tag, and that writes no revision"
docker start e2e-registry >/dev/null || fail "can't start the registry again"
registry_up
dw services deploy moving --image "$moving" --yes || fail "deploying the tag again failed"
moving_serves second
written=$(moving_revisions | jq '.revisions | length') || fail "services revisions failed"
docker stop e2e-registry >/dev/null || fail "can't stop the registry"
cant_pull second
[[ "$(moving_revisions | jq '.revisions | length')" == "$written" ]] || fail "a pull that failed wrote a revision: $(moving_revisions)"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after a pull that failed: $drift"

step "A moving tag: with its registry still gone, what isn't running comes back, from what the server has"
written=$(moving_revisions | jq '.revisions | length') || fail "services revisions failed"
moving_comes_back() {
  local said
  said=$(dw services deploy moving --image "$moving" --yes 2>&1) || fail "bringing back what $1 failed: $said"
  grep -qF "Can't pull $moving again" <<<"$said" || fail "it doesn't say it couldn't pull: $said"
  grep -qF "so moving runs the image this server has" <<<"$said" || fail "it doesn't say what it ran: $said"
  if grep -qF "stays as it is" <<<"$said"; then fail "it says it stays as it is, and it wasn't running: $said"; fi
  moving_running
  moving_serves second
}
moving_running() {
  [[ "$(docker inspect --format '{{.State.Running}}' "$(moving_id)")" == true ]] || fail "the app's container isn't running: $(docker ps -a)"
}
docker stop --time 1 "$(moving_id)" >/dev/null || fail "can't stop the app's container"
stopped_id=$(moving_id)
# A stop by hand isn't drift, so re-importing what's unchanged leaves it stopped. The
# CLI discards a plan with nothing in it, and says so on standard error.
exported=$(dw compose export) || fail "compose export failed"
planned=$(dw compose import --compose "$exported" --plan-only --json 2>"$moving_dir/import.err") ||
  fail "re-importing the export failed: $(cat "$moving_dir/import.err")"
grep -q 'Nothing to change' "$moving_dir/import.err" || fail "re-importing what's unchanged didn't say so: $(cat "$moving_dir/import.err")"
jq -e '[(.change.plan.servers // [])[].steps[]?] | length == 0' <<<"$planned" >/dev/null || fail "re-importing what's unchanged plans steps: $planned"
[[ "$(moving_id)" == "$stopped_id" && "$(docker inspect --format '{{.State.Running}}' "$stopped_id")" == false ]] || fail "re-importing what's unchanged started what's stopped"
# Asked to run it, the plan says it starts it, and nothing of a newer image.
planned=$(dw services deploy moving --image "$moving" --plan-only --json 2>/dev/null) || fail "planning the deploy of a stopped container failed"
jq -e '.change.plan.servers[0].steps[0].changes | any(.path == "state" and .from == "exited" and .to == "running")' <<<"$planned" >/dev/null ||
  fail "the plan's step doesn't start what's stopped: $planned"
jq -e '.change.plan.notes | any(.reason == "starts_stopped" and .message == "moving is stopped, so this starts it") and all(.reason != "pulls_again")' <<<"$planned" >/dev/null ||
  fail "the plan doesn't say it starts what's stopped: $planned"
dw changes discard "$(jq -r .change.id <<<"$planned")" >/dev/null || fail "changes discard failed"
moving_comes_back "was stopped"
docker rm --force "$(moving_id)" >/dev/null || fail "can't remove the app's container"
moving_comes_back "was removed"
[[ "$(moving_revisions | jq '.revisions | length')" == "$written" ]] || fail "bringing back the same image wrote a revision: $(moving_revisions)"

step "A moving tag: with its registry back and nothing newer, a deploy and a rollback each run what's stopped"
docker start e2e-registry >/dev/null || fail "can't start the registry again"
registry_up
docker stop --time 1 "$(moving_id)" >/dev/null || fail "can't stop the app's container"
before=$(moving_id)
said=$(dw services deploy moving --image "$moving" --yes 2>&1) || fail "deploying what's stopped failed: $said"
if grep -qF "stays as it is" <<<"$said"; then fail "it says it stays as it is, and it wasn't running: $said"; fi
moving_running
moving_serves second
# It's already what this asks for, so it's the same container that starts, as under
# docker compose up, and the step says that, rather than that it replaced it.
[[ "$(moving_id)" == "$before" ]] || fail "what's stopped was replaced, with nothing newer to replace it with: $said"
grep -qF "so the same container started" <<<"$said" || fail "it doesn't say the same container started: $said"
if grep -qF "Replaced service moving" <<<"$said"; then fail "it says it replaced what it only started: $said"; fi
docker stop --time 1 "$(moving_id)" >/dev/null || fail "can't stop the app's container"
same=$(moving_revisions | jq -r '[.revisions[1:][] | select(.matchesLatest)][0].number') || fail "services revisions failed"
[[ "$same" != null ]] || fail "no revision matches the latest: $(moving_revisions)"
said=$(dw services rollback moving --revision "$same" --yes 2>&1) || fail "rolling back what's stopped was refused: $said"
moving_running
moving_serves second
[[ "$(moving_revisions | jq '.revisions | length')" == "$written" ]] || fail "starting it again wrote a revision: $(moving_revisions)"

step "A moving tag: killed in the middle of a pull, the deploy finishes when Dockwell starts again"
digest_third=$(build_and_push third)
[[ "$digest_third" == sha256:* && "$digest_third" != "$digest_second" ]] || fail "pushing the third image said its digest is '$digest_third'"
docker tag "$moving_repo@$digest_second" "$moving" || fail "can't put the tag back on the second image"
docker rmi e2e-moving:third "$moving_repo@$digest_third" >/dev/null || fail "can't remove the third image here"
before=$(moving_id)
planned=$(dw services deploy moving --image "$moving" --plan-only --json 2>/dev/null) || fail "planning the deploy failed"
change=$(jq -r .change.id <<<"$planned")
# A paused registry holds the pull where it is, for Docker's 15 s or so.
docker pause e2e-registry >/dev/null || fail "can't pause the registry"
dw changes apply "$change" --yes >/dev/null 2>&1 &
applying=$!
for _ in $(seq 100); do
  [[ "$(dw changes get "$change" --json 2>/dev/null | jq -r '.change.steps[0].state // empty')" == STEP_STATE_RUNNING ]] && break
  sleep 0.1
done
[[ "$(dw changes get "$change" --json | jq -r '.change.steps[0].state // empty')" == STEP_STATE_RUNNING ]] ||
  fail "the deploy never reached its pull: $(dw changes get "$change" --json)"
kill_dockwell
wait "$applying" 2>/dev/null || true
docker unpause e2e-registry >/dev/null || fail "can't unpause the registry"
[[ "$(moving_id)" == "$before" ]] || fail "with Dockwell killed in the pull, the container was replaced"
moving_serves second
start_dockwell
wait_for_dockwell
for _ in $(seq 240); do
  state=$(dw changes get "$change" --json | jq -r .change.state) || fail "changes get failed"
  [[ "$state" != CHANGE_STATE_APPLYING ]] && break
  sleep 0.5
done
[[ "$state" == CHANGE_STATE_APPLIED ]] || fail "after starting again, the deploy is $state: $(dw changes get "$change" --json)"
[[ "$(moving_id)" != "$before" ]] || fail "the deploy reads applied, and the old container still runs"
moving_running
moving_serves third
runs=$(docker inspect --format '{{.Image}}' "$(moving_id)") || fail "docker inspect failed"
[[ "$runs" == "$(image_id_of "$moving_repo@$digest_third")" ]] || fail "the container runs $runs, want the third image"
revisions=$(moving_revisions) || fail "services revisions failed"
jq -e --arg written "$written" --arg third "$digest_third" --arg second "$digest_second" \
  '(.revisions | length) == (($written | tonumber) + 1) and .revisions[0].deployments[0].digest == $third and .revisions[0].deployments[0].state == "DEPLOYMENT_STATE_LIVE" and .revisions[1].deployments[0].digest == $second' \
  <<<"$revisions" >/dev/null || fail "the revisions don't say what happened, once: $revisions"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after starting again: $drift"

step "A moving tag: leaving a pin with nothing newer replaces the container, and says it will"
dw services deploy moving --image "$moving@$digest_third" --yes >/dev/null || fail "pinning the third image failed"
[[ "$(docker inspect --format '{{.Config.Image}}' "$(moving_id)")" == "$moving@$digest_third" ]] || fail "the app isn't pinned: $(docker ps -a)"
before=$(moving_id)
planned=$(dw services deploy moving --image "$moving" --plan-only --json 2>/dev/null) || fail "planning the deploy failed"
jq -e '.change.plan.notes | any(.reason == "leaves_pin") and all(.reason != "pulls_again")' <<<"$planned" >/dev/null || fail "the plan claims what it can't: $planned"
change=$(jq -r .change.id <<<"$planned")
read_plan=$(dw changes get "$change") || fail "changes get failed"
grep -qF "in place of the digest it's pinned to" <<<"$read_plan" || fail "the plan doesn't say it leaves the pin: $read_plan"
if grep -qE "if newer|only if" <<<"$read_plan"; then fail "the plan says it may not replace: $read_plan"; fi
said=$(dw changes apply "$change" --yes 2>&1) || fail "leaving the pin failed: $said"
if grep -qE "stays as it is|already the newest" <<<"$said"; then fail "it was replaced, and it says: $said"; fi
moving_did "$change" PLAN_ACTION_REPLACE
[[ "$(moving_id)" != "$before" ]] || fail "the container still names the pin"
moving_running
moving_serves third

dw services delete moving --yes || fail "services delete moving failed"
[[ -z "$(docker ps -aq --filter label=com.docker.compose.service=moving)" ]] || fail "deleting the app left its container"

step "An image built on this server deploys under its own tag, and a new build needs a tag of its own"
# Named for this registry, which has never heard of it, and not Docker Hub, which
# would then have to be up, so this is Docker's real answer for a name no registry has.
built=127.0.0.1:$registry_port/e2e-built-here:1
built_next=127.0.0.1:$registry_port/e2e-built-here:2
build_here() {
  printf '%s\n' "$1" >"$moving_dir/index.html"
  docker build -q -t "$built" "$moving_dir" >/dev/null || fail "can't build the image $1"
}
built_id() { docker ps -aq --no-trunc --filter label=com.docker.compose.service=built; }
build_here built-one
dw services create built --app-image "$built" --app-ports "$ports" --yes || fail "services create built failed"
moving_serves built-one
before=$(built_id)
planned=$(dw services deploy built --image "$built" --plan-only --json 2>/dev/null) || fail "planning the deploy failed"
change=$(jq -r .change.id <<<"$planned")
said=$(dw changes apply "$change" --yes 2>&1) || fail "deploying an image built here under its own tag failed: $said"
grep -qF "$built was built or loaded on this server" <<<"$said" || fail "with nothing built since, it says: $said"
moving_did "$change" PLAN_ACTION_NONE
[[ "$(built_id)" == "$before" ]] || fail "with nothing built since, the container was replaced"
# A rollback couldn't go back from it, so it's refused, and what runs stays.
build_here built-two
if said=$(dw services deploy built --image "$built" --yes 2>&1); then fail "a new build under the tag that runs deployed: $said"; fi
grep -qF "Give the build a tag of its own" <<<"$said" || fail "a new build under the tag that runs was refused saying: $said"
[[ "$(built_id)" == "$before" ]] || fail "a refused build replaced the container"
moving_serves built-one
docker tag "$built" "$built_next" || fail "can't give the build a tag of its own"
dw services deploy built --image "$built_next" --yes >/dev/null || fail "deploying the build under a tag of its own failed"
[[ "$(built_id)" != "$before" ]] || fail "under a tag of its own, the container wasn't replaced"
moving_serves built-two
revisions=$(dw services revisions built --json) || fail "services revisions failed"
jq -e '.revisions | length == 2' <<<"$revisions" >/dev/null || fail "a build under a tag of its own isn't one more revision: $revisions"
dw services delete built --yes || fail "services delete built failed"
docker rmi "$built" "$built_next" >/dev/null || fail "can't remove the images built here"
built=""
built_next=""

step "A multi-architecture tag: what ran is its index, and a build for another architecture leaves this server's image"
# One image, listed for each architecture, as a registry holds what buildx pushes.
# Each entry can name the one manifest: only this server's is ever pulled.
multi_repo=127.0.0.1:$registry_port/e2e-multi
multi_api=http://127.0.0.1:$registry_port/v2/e2e-multi
multi=$multi_repo:1
here=$(docker version --format '{{.Server.Arch}}') || fail "docker version failed"
printf 'multi\n' >"$moving_dir/index.html"
docker build -q -t "$multi_repo:one" "$moving_dir" >/dev/null || fail "can't build the image for a multi-architecture tag"
one=$(docker push "$multi_repo:one" | awk '/digest: /{ print $3 }')
[[ "$one" == sha256:* ]] || fail "pushing the image said its digest is '$one'"
# On containerd's image store a push makes an index already, so the entry is its first.
accept='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
# To a file, as a shell variable would lose a last newline, and with it the size.
pushed=$moving_dir/pushed.json
curl -fsS -H "Accept: $accept" -o "$pushed" "$multi_api/manifests/$one" || fail "can't read the pushed manifest"
if jq -e 'has("manifests")' "$pushed" >/dev/null; then
  entry=$(jq -c '.manifests[0] | {mediaType, digest, size}' "$pushed")
else
  entry=$(jq -cn --arg media "$(jq -r .mediaType "$pushed")" --arg digest "$one" --argjson size "$(wc -c <"$pushed")" '{mediaType: $media, digest: $digest, size: $size}')
fi
# push_index PLATFORMS TAG puts an index of those architectures under a tag, and says its digest.
push_index() {
  local file=$moving_dir/index.json
  jq -cn --argjson entry "$entry" --arg archs "$1" \
    '{schemaVersion: 2, mediaType: "application/vnd.oci.image.index.v1+json", manifests: [$archs | split(" ")[] | . as $arch | $entry + {platform: {os: "linux", architecture: $arch}}]}' >"$file"
  curl -fsS -o /dev/null -X PUT -H 'Content-Type: application/vnd.oci.image.index.v1+json' --data-binary "@$file" "$multi_api/manifests/$2" ||
    fail "the registry refused an index of $1"
  echo "sha256:$(sha256sum "$file" | cut -d' ' -f1)"
}
docker rmi "$multi_repo:one" >/dev/null || fail "can't remove the image here"
index_first=$(push_index "$here riscv64" 1)
multi_id() { docker ps -aq --no-trunc --filter label=com.docker.compose.service=multi; }
dw services create multi --app-image "$multi" --app-ports "$ports" --yes || fail "services create multi failed"
moving_serves multi
ran=$(dw services revisions multi --json | jq -r '.revisions[0].deployments[0].digest') || fail "services revisions failed"
[[ "$ran" == "$index_first" ]] || fail "a multi-architecture tag ran $ran, want its index, $index_first"

# Its image ID is this server's own image, unless containerd keeps them, where it's
# the index, which is what ADR 0021 accepts.
index_second=$(push_index "$here riscv64 ppc64le" 1)
[[ "$index_second" != "$index_first" ]] || fail "the second index is the first"
store=$(docker info --format '{{range .DriverStatus}}{{println .}}{{end}}') || fail "docker info failed"
want=PLAN_ACTION_NONE
[[ "$store" != *io.containerd.snapshotter* ]] || want=PLAN_ACTION_REPLACE
echo "docker-e2e: Docker's image store says: $store, so a build for another architecture should do $want"
before=$(multi_id)
planned=$(dw services deploy multi --image "$multi" --plan-only --json 2>/dev/null) || fail "planning the deploy failed"
change=$(jq -r .change.id <<<"$planned")
said=$(dw changes apply "$change" --yes 2>&1) || fail "deploying a build for another architecture failed: $said"
moving_did "$change" "$want"
if [[ "$want" == PLAN_ACTION_NONE ]]; then
  [[ "$(multi_id)" == "$before" ]] || fail "a build for another architecture replaced this server's container"
fi
moving_serves multi

# Docker's own words for a tag with nothing for this server, which only it can say.
push_index riscv64 other >/dev/null
if said=$(dw services deploy multi --image "$multi_repo:other" --yes 2>&1); then fail "a tag with no build for this server deployed: $said"; fi
[[ "$said" == *"has no build for this server's platform"* ]] || fail "a tag with no build for this server failed some other way: $said"
moving_serves multi
dw services delete multi --yes || fail "services delete multi failed"
docker rmi "$multi" "$multi_repo@$index_first" "$multi_repo@$index_second" >/dev/null 2>&1 || true
moving_tidy

step "Ingress: Pebble, Let's Encrypt's test CA, on this machine"
in_ingress=1
tools=$("$(dirname "$0")/ingress-tools.sh" "$tools") || fail "can't get Caddy and Pebble"
domain=app.e2e-dockwell.example.net
cat >"$pebble_dir/pebble.json" <<PEBBLE
{"pebble":{"listenAddress":"127.0.0.1:14000","managementListenAddress":"127.0.0.1:15000",
"certificate":"$pebble_dir/cert.pem","privateKey":"$pebble_dir/key.pem","httpPort":80,"tlsPort":443,
"ocspResponderURL":"","externalAccountBindingRequired":false}}
PEBBLE
"$tools/pebble-challtestsrv" -defaultIPv4 127.0.0.1 -defaultIPv6 "" -dnsserver 127.0.0.1:8053 -doh "" -http01 "" -https01 "" -tlsalpn01 "" \
  -management 127.0.0.1:8055 >"$pebble_dir/challtest.out" 2>&1 &
pebble_pids+=("$!")
PEBBLE_VA_NOSLEEP=1 PEBBLE_WFE_NONCEREJECT=0 "$tools/pebble" -config "$pebble_dir/pebble.json" -dnsserver 127.0.0.1:8053 >"$pebble_dir/pebble.out" 2>&1 &
pebble_pids+=("$!")
for _ in $(seq 100); do
  curl -fsS --cacert "$pebble_dir/cert.pem" https://localhost:14000/dir >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS --cacert "$pebble_dir/cert.pem" https://localhost:14000/dir >/dev/null || fail "Pebble didn't answer within 10 s"

listening() { [[ -n "$(ss -H -ltn "sport = :$1")" ]]; }
if listening 80 || listening 443; then
  fail "something on this machine already holds port 80 or 443, so the ingress's part can't run. Stop it, and run this again"
fi

dw services create web --app-image nginx:1.27-alpine --app-env GREETING=hello --yes || fail "services create web failed"
web=$(docker ps --filter label=com.docker.compose.service=web --format '{{.Names}}') || fail "docker ps failed"
web_id=$(docker inspect --format '{{.Id}}' "$web") || fail "docker inspect $web failed"

step "Ingress: taken ports are refused at plan time, by name"
docker run -d --name e2e-holds-80 -p 80:80 nginx:1.27-alpine >/dev/null || fail "can't run a container on port 80"
set +e
refusal=$(dw routes create www --domain "$domain" --service web --port 80 --yes 2>&1)
status=$?
set -e
[[ "$status" != 0 ]] || fail "a route went through with port 80 taken"
grep -q 'the container e2e-holds-80' <<<"$refusal" || fail "the refusal doesn't name what holds port 80: $refusal"
grep -q 'point that proxy at it' <<<"$refusal" || fail "the refusal doesn't say how to fix it: $refusal"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress started with port 80 taken"; fi
docker rm --force e2e-holds-80 >/dev/null || fail "can't remove the container on port 80"

step "Ingress: a server's first route starts Caddy, with nothing else stopping"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress ran before any route"; fi
# Stopped by hand, and Docker starts it again when it next restarts, as 1Panel's
# OpenResty is, so the plan says so.
docker run -d --name e2e-returns --restart=always -p 80:80 nginx:1.27-alpine >/dev/null || fail "can't run a container on port 80"
docker stop --time 1 e2e-returns >/dev/null || fail "can't stop e2e-returns"
# docker stop can return before the list of what's running has caught up, and that's
# what a plan reads for who holds a port.
for _ in $(seq 100); do
  [[ -n "$(docker ps --quiet --filter name=^e2e-returns\$)" ]] || break
  sleep 0.1
done
first=$(dw routes create www --domain "$domain" --service web --port 80 --yes) || fail "routes create www failed: $first"
grep -q 'Docker starts e2e-returns again' <<<"$first" || fail "the plan doesn't warn of a stopped container that comes back: $first"
grep -q 'docker update --restart=no e2e-returns' <<<"$first" || fail "the plan doesn't say how to keep it stopped: $first"
docker rm --force e2e-returns >/dev/null || fail "can't remove e2e-returns"
[[ "$(docker inspect --format '{{.Id}}' "$web")" == "$web_id" ]] || fail "adding a route made the service's container again, which is downtime"
hardening=$(docker inspect --format '{{.HostConfig.NetworkMode}} {{.HostConfig.ReadonlyRootfs}} {{.HostConfig.RestartPolicy.Name}}' dockwell-ingress) ||
  fail "there's no dockwell-ingress after the first route"
[[ "$hardening" == "host true unless-stopped" ]] || fail "dockwell-ingress runs as: $hardening, want the host network, a read-only root, and unless-stopped"
capabilities=$(docker inspect --format '{{.HostConfig.CapDrop}} {{.HostConfig.CapAdd}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
[[ "$capabilities" == *ALL*NET_BIND_SERVICE* ]] || fail "dockwell-ingress has the capabilities $capabilities, want all dropped but NET_BIND_SERVICE"
image=$(docker inspect --format '{{.Config.Image}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
[[ "$image" == caddy@sha256:* ]] || fail "dockwell-ingress runs $image, want it by digest"
user=$(docker inspect --format '{{.Config.User}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
if [[ -n "${E2E_ROOT:-}" ]]; then
  caddy_uid=65532
  caddy_user=65532:65532
else
  caddy_uid=$(id -u)
  caddy_user=$(id -u):$(id -g)
fi
[[ "$user" == "$caddy_user" ]] || fail "dockwell-ingress runs as $user, want $caddy_user"
says true docker inspect --format '{{.State.Running}}' dockwell-ingress || fail "dockwell-ingress isn't running"
running_as=$(docker exec dockwell-ingress id -u) || fail "can't ask dockwell-ingress who it runs as"
[[ "$running_as" == "$caddy_uid" ]] || fail "Caddy's process runs as uid $running_as, want $caddy_uid"
limits=$(docker inspect --format '{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.UsernsMode}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
[[ "$limits" == "1073741824 1073741824 host" ]] || fail "dockwell-ingress has memory, swap and user namespace $limits, want 1 GiB, no swap and the server's"
says '^CADDY_ADMIN=unix//run/dockwell/admin\.sock\|0600$' docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' dockwell-ingress ||
  fail "dockwell-ingress has no CADDY_ADMIN naming its socket, so an empty last config would open the admin API on TCP"
for dir in data config run; do
  owned=$(priv stat -c '%u %a' "$data/ingress/$dir") || fail "can't read $data/ingress/$dir"
  [[ "$owned" == "$caddy_uid 700" ]] || fail "ingress/$dir is owned and has mode $owned, want $caddy_uid 700"
done
if [[ -n "${E2E_ROOT:-}" ]]; then
  # What Caddy reads as it starts is root's, so nothing it does can change where a
  # write of root's lands, or what it reads next time.
  for file in boot boot/bootstrap.json boot/acme-roots.pem; do
    owned=$(priv stat -c '%u' "$data/ingress/$file") || fail "can't read $data/ingress/$file"
    [[ "$owned" == 0 ]] || fail "ingress/$file is owned by uid $owned, want root"
  done
  [[ "$(priv stat -c '%u %a' "$data/ingress/last-good.json")" == "0 600" ]] || fail "the agent's copy of Caddy's config isn't root's alone"
fi
if docker exec dockwell-ingress touch /etc/dockwell/x 2>/dev/null; then fail "Caddy can write to what it reads as it starts"; fi
for _ in $(seq 100); do
  says true docker inspect --format '{{.State.Running}}' dockwell-ingress-keep 2>/dev/null && break
  sleep 0.1
done
says true docker inspect --format '{{.State.Running}}' dockwell-ingress-keep 2>/dev/null || fail "nothing keeps the ingress network from being pruned: $(docker logs dockwell-ingress-keep 2>&1)"
for p in 80 443; do
  listening "$p" || fail "nothing listens on port $p"
done
[[ -n "$(ss -H -lun 'sport = :443')" ]] || fail "nothing listens on UDP 443, for HTTP/3"
curl -fsS --max-time 2 http://127.0.0.1:2019/config/ >/dev/null 2>&1 && fail "Caddy's admin API answers on TCP"
priv test -S "$data/ingress/run/admin.sock" || fail "there's no admin socket at $data/ingress/run/admin.sock"

step "Ingress: the network is kept to itself"
network=$(docker network inspect --format '{{.Internal}} {{index .Options "com.docker.network.bridge.enable_icc"}}' dockwell_ingress) ||
  fail "there's no network dockwell_ingress"
[[ "$network" == "true false" ]] || fail "dockwell_ingress is internal and enable_icc: $network, want true and false"
address=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAddress}}' "$web") || fail "web isn't on dockwell_ingress"
static=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAMConfig.IPv4Address}}' "$web") || fail "web has no static address on dockwell_ingress"
[[ "$address" == "$static" ]] || fail "web is at $address, and asked for $static"
says nginx curl -fsS --max-time 5 "http://$address/" || fail "this machine can't reach web on the ingress network, at $address, as Caddy must"
# wget_from_network says how wget exited inside a container on the network, so a
# docker run that fails for any other reason doesn't read as the network holding.
wget_from_network() {
  docker run --rm --network dockwell_ingress nginx:1.27-alpine sh -c "wget -q -T 3 -O- $1 >/dev/null 2>&1; echo wget exited \$?"
}
reached=$(wget_from_network "http://$address/") || fail "can't run a container on the ingress network: $reached"
[[ "$reached" == 'wget exited '[1-9]* ]] || fail "a container on the ingress network reached web, as $reached: enable_icc: false isn't keeping them apart"
reached=$(wget_from_network http://1.1.1.1/) || fail "can't run a container on the ingress network: $reached"
[[ "$reached" == 'wget exited '[1-9]* ]] || fail "a container on the ingress network got out through it, as $reached: internal: true isn't holding"
outbound=$(docker exec "$web" wget -q -T 5 --spider http://localhost/ 2>&1 && echo up) || fail "web stopped answering itself: $outbound"
default_route=$(docker exec "$web" ip route show default 2>/dev/null) || default_route=""
ingress_gateway=$(docker network inspect --format '{{(index .IPAM.Config 0).Gateway}}' dockwell_ingress) || ingress_gateway=""
if [[ -n "$ingress_gateway" ]] && grep -qF "$ingress_gateway" <<<"$default_route"; then
  fail "web's default route moved to the ingress network: $default_route"
fi

step "Ingress: a real certificate, from a real CA, and a request over HTTPS to a real container"
state=""
for _ in $(seq 120); do
  listed=$(dw domains list --json 2>/dev/null) || listed='{}'
  state=$(jq -r --arg d "$domain" '.domains[]? | select(.name == $d) | .servers[0].status.certificate.state // empty' <<<"$listed")
  [[ "$state" == CERTIFICATE_STATE_ISSUED ]] && break
  sleep 0.5
done
[[ "$state" == CERTIFICATE_STATE_ISSUED ]] || fail "$domain's certificate reads '$state' after 60 s, want issued"
curl -fsS --cacert "$pebble_dir/cert.pem" https://localhost:15000/roots/0 >"$pebble_dir/root.pem" || fail "can't read Pebble's root"
for intermediate in 0 1; do
  curl -fsS --cacert "$pebble_dir/cert.pem" "https://localhost:15000/intermediates/$intermediate" >>"$pebble_dir/root.pem" 2>/dev/null || true
done
over_https() { curl -fsS --max-time 10 --cacert "$pebble_dir/root.pem" --resolve "$domain:443:127.0.0.1" "$@" "https://$domain/"; }
says nginx over_https || fail "https://$domain/ doesn't serve web through Caddy: $(over_https -v 2>&1)"
redirect=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code} %{redirect_url}' --resolve "$domain:80:127.0.0.1" "http://$domain/x?y=1") || fail "http://$domain/ didn't answer"
[[ "$redirect" == "308 https://$domain/x?y=1" ]] || fail "http://$domain/x?y=1 answers $redirect, want a redirect to HTTPS"

step "Ingress: the service reads the real client, and Caddy owns the header"
over_https -H 'X-Forwarded-For: 6.6.6.6' >/dev/null || fail "the request with a forged header failed"
sleep 0.5
last_log() { docker logs --tail 5 "$web" 2>&1 | tail -1; }
says '"127\.0\.0\.1"$' last_log ||
  fail "web's last request doesn't read as from 127.0.0.1, in X-Forwarded-For: $(docker logs --tail 5 "$web" 2>&1)"
logs=$(docker logs --tail 5 "$web" 2>&1) || fail "docker logs $web failed"
if grep -q '6\.6\.6\.6' <<<"$logs"; then fail "a forged X-Forwarded-For reached web"; fi
# Whether this machine has IPv6 on loopback is the kernel's to say, as a curl that
# fails for any other reason must fail this.
if grep -q '^00000000000000000000000000000001' /proc/net/if_inet6 2>/dev/null; then
  curl -fsS --max-time 5 --cacert "$pebble_dir/root.pem" --resolve "$domain:443:[::1]" -o /dev/null "https://$domain/" ||
    fail "https://$domain/ doesn't answer over IPv6"
  sleep 0.5
  says '"::1"$' last_log ||
    fail "over IPv6, web's last request doesn't read as from ::1, so a client's real address is lost: $(docker logs --tail 5 "$web" 2>&1)"
else
  echo "This machine has no IPv6 on loopback, so the client's real address over IPv6 wasn't checked"
fi

step "Ingress: a replaced service moves to a container and an address of its own, and goes on serving"
dw services update web --app-image nginx:1.27-alpine --app-env GREETING=hi --yes || fail "services update web failed"
replaced=$web
web=$(docker ps -a --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=web --format '{{.Names}}') || fail "docker ps failed"
[[ "$web" == "$readable.web-2" ]] || fail "after the update, Docker has '$web' for web, want only $readable.web-2: a rolling replace takes the next number, as docker compose would give it"
if docker inspect "$replaced" >/dev/null 2>&1; then fail "the update left the old container, $replaced"; fi
now=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAMConfig.IPv4Address}}' "$web") || fail "the new container isn't on dockwell_ingress"
[[ -n "$now" && "$now" != "$static" ]] || fail "the new container is at '$now', want an address of its own, not the old one's, $static, which a connection that outlives the rollout may still dial"
static=$now
says nginx over_https || fail "after the update, https://$domain/ doesn't serve web"
listed=$(docker compose -p "$project" ps --format '{{.Service}}') || fail "docker compose -p $project ps failed"
grep -qx web <<<"$listed" || fail "docker compose doesn't list web on the ingress network: $listed"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e '(.drift // []) | length == 0' <<<"$drift" >/dev/null || fail "drift after a route and a replace: $drift"

step "Ingress: traffic keeps flowing with Dockwell down, and through a restart of Caddy"
stop_dockwell
says nginx over_https || fail "with Dockwell down, https://$domain/ stopped serving"
docker restart --time 5 dockwell-ingress >/dev/null || fail "docker restart dockwell-ingress failed"
for _ in $(seq 100); do
  over_https >/dev/null 2>&1 && break
  sleep 0.1
done
says nginx over_https || fail "with Dockwell down, Caddy came back without its last config"
# Docker itself restarting brings back everything, and gives each its address, with
# no agent there. It needs root, and a Docker that systemd runs.
if [[ -n "${E2E_ROOT:-}" ]] && systemctl is-active --quiet docker 2>/dev/null; then
  step "Ingress: traffic comes back with Docker itself restarted, and Dockwell down"
  sudo -n systemctl restart docker || fail "can't restart Docker"
  for _ in $(seq 300); do
    over_https >/dev/null 2>&1 && break
    sleep 0.1
  done
  says nginx over_https || fail "with Docker restarted and Dockwell down, https://$domain/ didn't come back"
  now=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAMConfig.IPv4Address}}' "$web") || fail "web isn't on dockwell_ingress after Docker restarted"
  [[ "$now" == "$static" ]] || fail "after Docker restarted, web is at $now, want $static"
else
  echo "Docker wasn't restarted, as that needs E2E_ROOT and systemd. CI has both"
fi
start_dockwell
wait_for_dockwell

autosave=$data/ingress/config/caddy/autosave.json
serves() {
  for _ in $(seq 300); do
    over_https >/dev/null 2>&1 && break
    sleep 0.1
  done
  says nginx over_https || fail "$1"
}
admin() { priv curl -fsS --max-time 2 --unix-socket "$data/ingress/run/admin.sock" "http://caddy$1"; }

step "Ingress: a last config left empty comes back, with the admin API on its socket alone"
stop_dockwell
docker stop --time 5 dockwell-ingress >/dev/null || fail "docker stop dockwell-ingress failed"
priv truncate -s 0 "$autosave" || fail "can't empty $autosave"
docker start dockwell-ingress >/dev/null || fail "docker start dockwell-ingress failed"
for _ in $(seq 100); do
  admin /config/ >/dev/null 2>&1 && break
  sleep 0.1
done
[[ "$(admin /config/)" == null ]] || fail "from an empty last config, Caddy runs: $(admin /config/)"
if curl -fsS --max-time 2 http://127.0.0.1:2019/config/ >/dev/null 2>&1; then fail "from an empty last config, Caddy's admin API answers on TCP"; fi
start_dockwell
wait_for_dockwell
serves "the agent didn't put back Caddy's last config, left empty"
[[ "$(priv stat -c '%u' "$autosave")" == "$caddy_uid" ]] || fail "the last config is owned by uid $(priv stat -c '%u' "$autosave"), want $caddy_uid"

step "Ingress: a last config cut short comes back too, rather than restarting Caddy for ever"
stop_dockwell
docker stop --time 5 dockwell-ingress >/dev/null || fail "docker stop dockwell-ingress failed"
priv truncate -s 100 "$autosave" || fail "can't cut $autosave short"
docker start dockwell-ingress >/dev/null 2>&1 || true
sleep 2
if over_https >/dev/null 2>&1; then fail "Caddy served from half a config, so this proves nothing"; fi
start_dockwell
wait_for_dockwell
serves "the agent didn't put back Caddy's last config, cut short"

step "Ingress: a last config that has gone comes back, rather than Caddy being stopped as a server with no routes"
stop_dockwell
docker stop --time 5 dockwell-ingress >/dev/null || fail "docker stop dockwell-ingress failed"
priv rm -f "$autosave" || fail "can't remove $autosave"
docker start dockwell-ingress >/dev/null || fail "docker start dockwell-ingress failed"
for _ in $(seq 100); do
  admin /config/ >/dev/null 2>&1 && break
  sleep 0.1
done
admin /config/ >/dev/null || fail "Caddy didn't start from its bootstrap"
if over_https >/dev/null 2>&1; then fail "Caddy served from its bootstrap, so this proves nothing"; fi
start_dockwell
wait_for_dockwell
serves "the agent didn't put back Caddy's last config, gone"
priv test -f "$autosave" || fail "Caddy's last config wasn't written again"

step "Docker's events say a container it started again went past its memory limit, which the agent reads of Caddy"
# Not Caddy itself, which the agent makes again in the moment it isn't running. The
# agent says why every site dropped from what this reads (docs/security/ingress.md),
# as Docker clears OOMKilled as it starts a container again.
oom_from=$(date +%s)
docker run -d --name e2e-oom --memory 16m --memory-swap 16m --restart unless-stopped nginx:1.27-alpine sh -c 'sleep 2; tail /dev/zero' >/dev/null ||
  fail "can't run e2e-oom"
restarted=""
for _ in $(seq 300); do
  state=$(docker inspect --format '{{.RestartCount}} {{.State.Running}} {{.State.Restarting}}' e2e-oom) || fail "docker inspect e2e-oom failed"
  read -r restarts running restarting <<<"$state"
  if ((restarts >= 1)) && [[ "$running" == true && "$restarting" == false ]]; then
    restarted=yes
    break
  fi
  sleep 0.1
done
[[ -n "$restarted" ]] || fail "e2e-oom wasn't started again: $state (restarts, running, restarting)"
# Docker can show it running again before it has the event, which the agent allows for.
oom=""
for _ in $(seq 40); do
  oom=$(docker events --since "$oom_from" --until "$(date +%s.%N)" --filter container=e2e-oom --filter event=oom --format '{{.Action}}') || fail "docker events failed"
  [[ -z "$oom" ]] || break
  sleep 0.5
done
docker rm --force e2e-oom >/dev/null || fail "can't remove e2e-oom"
[[ "$oom" == *oom* ]] || fail "Docker's events say nothing of a container going past its memory limit, so the agent wouldn't say why every site dropped: '$oom'"

step "Ingress: docker network prune can't take the network, and a network lost comes back on its subnet"
subnet=$(docker network inspect --format '{{(index .IPAM.Config 0).Subnet}}' dockwell_ingress) || fail "there's no network dockwell_ingress"
stop_dockwell
docker stop --time 1 "$web" >/dev/null || fail "docker stop $web failed"
docker network prune --force >/dev/null || fail "docker network prune failed"
docker network inspect dockwell_ingress >/dev/null 2>&1 || fail "docker network prune took the ingress network, with the keeper on it"
docker rm --force dockwell-ingress-keep >/dev/null || fail "can't remove the keeper"
docker network prune --force >/dev/null || fail "docker network prune failed"
if docker network inspect dockwell_ingress >/dev/null 2>&1; then fail "docker network prune left the network with no running container on it, so this proves nothing"; fi
start_dockwell
wait_for_dockwell
for _ in $(seq 300); do
  [[ "$(docker inspect --format '{{.State.Running}}' dockwell-ingress-keep 2>/dev/null)" == true ]] && break
  sleep 0.1
done
[[ "$(docker inspect --format '{{.State.Running}}' dockwell-ingress-keep 2>/dev/null)" == true ]] || fail "the keeper didn't come back"
[[ "$(docker network inspect --format '{{(index .IPAM.Config 0).Subnet}}' dockwell_ingress)" == "$subnet" ]] || fail "the network came back off its subnet, $subnet: $(docker network inspect dockwell_ingress)"
# A stopped container names the old network's ID, and can't start until it comes
# off it, which the agent has seen to.
docker start "$web" >/dev/null || fail "web can't start on the network made again"
now=$(docker inspect --format '{{(index .NetworkSettings.Networks "dockwell_ingress").IPAMConfig.IPv4Address}}' "$web") || fail "web isn't on dockwell_ingress"
[[ "$now" == "$static" ]] || fail "on the network made again, web is at $now, want $static"
serves "on the network made again, https://$domain/ doesn't serve web"

step "Ingress: another network on its subnet closes the routes, names it, and reuses nothing"
stop_dockwell
docker stop --time 1 "$web" >/dev/null || fail "docker stop $web failed"
docker rm --force dockwell-ingress-keep >/dev/null || fail "can't remove the keeper"
docker network rm dockwell_ingress >/dev/null || fail "can't remove dockwell_ingress"
docker network create --subnet "$subnet" e2e-took-subnet >/dev/null || fail "can't make a network on $subnet"
start_dockwell
wait_for_dockwell
code=""
for _ in $(seq 300); do
  code=$(curl -sS --max-time 5 --cacert "$pebble_dir/root.pem" --resolve "$domain:443:127.0.0.1" -o /dev/null -w '%{http_code}' "https://$domain/" 2>/dev/null) || code=""
  [[ "$code" == 503 ]] && break
  sleep 0.1
done
[[ "$code" == 503 ]] || fail "with another network on its subnet, https://$domain/ answers $code, want 503"
ingress_problem() { dw servers list --json 2>/dev/null | jq -r '.servers[0].ingress'; }
said=""
for _ in $(seq 300); do
  said=$(ingress_problem)
  jq -e '.problem == "INGRESS_PROBLEM_SUBNET_TAKEN"' <<<"$said" >/dev/null 2>&1 && break
  sleep 0.1
done
jq -e '.problem == "INGRESS_PROBLEM_SUBNET_TAKEN"' <<<"$said" >/dev/null || fail "the API doesn't say the subnet is taken: $said"
jq -e '.message | contains("e2e-took-subnet")' <<<"$said" >/dev/null || fail "the API doesn't name who took it: $said"
if docker network inspect dockwell_ingress >/dev/null 2>&1; then fail "the network was made anyway"; fi
docker network rm e2e-took-subnet >/dev/null || fail "can't remove e2e-took-subnet"
stop_dockwell
start_dockwell
wait_for_dockwell
for _ in $(seq 300); do
  docker network inspect dockwell_ingress >/dev/null 2>&1 && docker start "$web" >/dev/null 2>&1 && break
  sleep 0.1
done
serves "with its subnet free, https://$domain/ doesn't serve web"

step "Ingress: a proxy back on the ports is named, Caddy isn't made again for it, and a route can still go"
dw routes create api --domain "$domain" --path /api --service web --port 80 --yes >/dev/null || fail "routes create api failed"
stop_dockwell
docker stop --time 5 dockwell-ingress >/dev/null || fail "docker stop dockwell-ingress failed"
caddy_id=$(docker inspect --format '{{.Id}}' dockwell-ingress) || fail "docker inspect dockwell-ingress failed"
docker run -d --name e2e-holds-80 --restart=always -p 80:80 nginx:1.27-alpine >/dev/null || fail "can't run a container on port 80"
start_dockwell
wait_for_dockwell
said=""
for _ in $(seq 300); do
  said=$(ingress_problem)
  jq -e '.problem == "INGRESS_PROBLEM_PORTS_TAKEN"' <<<"$said" >/dev/null 2>&1 && break
  sleep 0.1
done
jq -e '.problem == "INGRESS_PROBLEM_PORTS_TAKEN" and (.holders | index("e2e-holds-80") != null)' <<<"$said" >/dev/null || fail "the API doesn't name what holds the ports: $said"
grep -q 'docker update --restart=no e2e-holds-80' "$log" || fail "the log doesn't name what holds the ports, with the fix"
[[ "$(docker inspect --format '{{.Id}}' dockwell-ingress)" == "$caddy_id" ]] || fail "dockwell-ingress was made again with the ports taken"
# Removing a route must always work, and doesn't start Caddy to do it.
dw routes delete api --yes >/dev/null || fail "a route couldn't be removed with another proxy on the ports"
if priv grep -q '/api' "$autosave"; then fail "the route removed with Caddy down is still in its last config"; fi
docker rm --force e2e-holds-80 >/dev/null || fail "can't remove e2e-holds-80"
stop_dockwell
start_dockwell
wait_for_dockwell
serves "with the ports free again, https://$domain/ doesn't serve web"

step "Ingress: a hand edit reads as drift, and a revert puts it back"
hand='{"handler":"static_response","body":"by hand"}'
priv curl -fsS --unix-socket "$data/ingress/run/admin.sock" -X PUT -H 'Content-Type: application/json' -d "$hand" \
  "http://caddy/id/dockwell.$project.www/handle/0" >/dev/null || fail "can't edit the route by hand"
says 'by hand' over_https || fail "the hand edit didn't take"
drifted=""
for _ in $(seq 60); do
  drift=$(dw drift list --json 2>/dev/null) || drift='{}'
  drifted=$(jq -r '.drift[]? | select(.resource.kind == "route") | .resourceId' <<<"$drift")
  [[ -n "$drifted" ]] && break
  sleep 0.5
done
[[ -n "$drifted" ]] || fail "a hand edit to the route doesn't read as drift: $drift"
says 'by hand' over_https || fail "Dockwell reverted a hand edit on its own: drift is reported, never silently reverted"
server=$(jq -r '.drift[] | select(.resource.kind == "route") | .serverId' <<<"$drift")
dw drift revert --server-id "$server" --resource-id "$drifted" --yes || fail "drift revert failed"
says nginx over_https || fail "after reverting, https://$domain/ doesn't serve web"

in_rollout=1
# A step says when, to read against when the load says a request failed.
rollout_step() { step "Rollouts ($(date +%T)): $*"; }

# The network prune above took the imported file's network, as nothing was on it by
# then, and it rightly stays as drift. So a rollout is held to what it adds.
drifted() { dw drift list --json | jq -c '[(.drift // [])[] | .resource] | sort_by(.kind, .name)'; }
drift_before=$(drifted) || fail "drift list failed"
# What it holds is that network, and nothing else, or this would let through drift
# from something it never looked at.
jq -e 'length == 1 and .[0].kind == "network" and .[0].name == "back"' <<<"$drift_before" >/dev/null ||
  fail "before the rollouts, the drift should be the network back alone, and is: $drift_before"

rollout_step "One app, in an image slow to turn healthy and in one that never does"
(cd "$(dirname "$0")/.." && CGO_ENABLED=0 go build -o "$rollout_dir/e2e-load" ./scripts/e2e-load) || fail "can't build scripts/e2e-load"
build_app() {
  cat >"$rollout_dir/Dockerfile" <<DOCKERFILE
FROM scratch
COPY e2e-load /e2e-load
ENV $2
ENTRYPOINT ["/e2e-load", "serve"]
DOCKERFILE
  docker build --quiet --tag "dockwell-e2e-app:$1" "$rollout_dir" >/dev/null || fail "can't build the image dockwell-e2e-app:$1"
}
# It answers 503 for this long, so a check has to wait for it.
slow_for=8
never_says="the database refuses connections"
build_app slow "E2E_READY_AFTER=${slow_for}s"
build_app never "E2E_NEVER_READY=\"$never_says\""

# The check the agent makes. The start period is longer than the slow image takes,
# so its failures before then don't count.
http_check=(--app-healthcheck-http-port 8080 --app-healthcheck-http-path /healthz --app-healthcheck-interval 1s --app-healthcheck-timeout 2s --app-healthcheck-retries 3 --app-healthcheck-start-period 15s)
# The one Docker runs, inside the container, as a Compose file has it.
docker_check=(--app-healthcheck-test CMD --app-healthcheck-test /e2e-load --app-healthcheck-test check --app-healthcheck-interval 1s --app-healthcheck-timeout 3s --app-healthcheck-retries 3 --app-healthcheck-start-period 1s)
# app_spec is the whole spec of a deploy: its image, what makes it different from
# the last, and its check, which is the agent's unless it is given another.
app_spec() {
  local image=$1 deploy=$2
  shift 2
  if [[ $# == 0 ]]; then set -- "${http_check[@]}"; fi
  printf '%s\n' --app-image "dockwell-e2e-app:$image" --app-env "DEPLOY=$deploy" --app-rollout-drain 10s "$@"
}
deploy_app() {
  local spec=()
  mapfile -t spec < <(app_spec "$@")
  dw services update app "${spec[@]}" --yes
}
app_containers() {
  docker ps -a --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=app --format '{{.Names}} {{.State}} {{.Image}}'
}
# one_app checks that all there is of the app is one running container of an
# image, by the names docker compose reads, and sets app to its name.
one_app() {
  local list state image
  list=$(app_containers) || fail "docker ps failed"
  [[ "$(wc -l <<<"$list")" == 1 ]] || fail "$1: Docker has more than one container of app, or none: $list"
  read -r app state image <<<"$list"
  [[ "$state" == running ]] || fail "$1: $app is $state, want running"
  [[ "$image" == "dockwell-e2e-app:$2" ]] || fail "$1: $app runs $image, want dockwell-e2e-app:$2"
  [[ "$app" =~ ^default\.production\.app-[0-9]+$ ]] || fail "$1: the container is called $app, want default.production.app-<number>, named after its project and environment"
  [[ "$(docker inspect --format '{{index .Config.Labels "com.docker.compose.container-number"}}' "$app")" == "${app##*-}" ]] ||
    fail "$1: $app's number label isn't what its name says: $(docker inspect --format '{{json .Config.Labels}}' "$app")"
  [[ "$(docker compose -p "$project" ps --format '{{.Service}} {{.Name}}' | grep '^app ')" == "app $app" ]] ||
    fail "$1: docker compose reads something else: $(docker compose -p "$project" ps --format '{{.Service}} {{.Name}}')"
}
app_env() { docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$1"; }
# newest_change prints the newest change as JSON.
newest_change() { dw changes list --json | jq -c '.changes[0]'; }
changes_count() { dw changes list --page-size 200 --json | jq '.changes | length'; }
# rolled_back waits for the rollback a failed deploy started, as the CLI returns
# with it still going, and prints it.
rolled_back() {
  local change=""
  for _ in $(seq 240); do
    change=$(newest_change) || change=""
    if jq -e '(.summary | startswith("Roll back")) and .state != "CHANGE_STATE_APPLYING" and .state != "CHANGE_STATE_PLANNED"' <<<"$change" >/dev/null 2>&1; then
      echo "$change"
      return 0
    fi
    sleep 0.5
  done
  fail "120 s after a deploy failed, the newest change isn't a finished rollback: $change"
}

# moves_with_its_route takes the app and its route to a port in one change, as a client
# sends it. A route's step comes after its service's, so nothing but the rollout can
# take the route there before the old container, and with it the old port, goes.
moves_with_its_route() {
  local port=$1 deploy=$2 rollout=${3:-'{"drain":"10s"}'} service route change
  service=$(jq -nc --arg port "$port" --arg deploy "$deploy" --argjson rollout "$rollout" '{resource: {kind: "service", name: "app"}, spec: {service: {app: {
    image: "dockwell-e2e-app:slow", env: {DEPLOY: $deploy, E2E_PORT: $port}, rollout: $rollout,
    healthcheck: {http: {port: ($port | tonumber), path: "/healthz"}, interval: "1s", timeout: "2s", retries: 3, startPeriod: "15s"}}}}}') ||
    fail "can't write the app's edit"
  route=$(jq -nc --arg port "$port" --arg domain "$roll_domain" '{resource: {kind: "route", name: "roll"}, spec: {route: {domain: $domain, service: "app", port: ($port | tonumber)}}}') ||
    fail "can't write the route's edit"
  dw changes propose --summary "Move app to port $port" --edits "$service" --edits "$route" --yes ||
    fail "moving the app and its route to port $port in one change failed"
  one_app "after moving to port $port" slow
  app_env "$app" | grep -qx "E2E_PORT=$port" || fail "$app should listen on $port, and has: $(app_env "$app")"
  change=$(newest_change) || fail "changes list failed"
  jq -e '.state == "CHANGE_STATE_APPLIED"' <<<"$change" >/dev/null || fail "moving to port $port reads as: $change"
  dw changes get "$(jq -r .id <<<"$change")" --json | jq -e --arg port "$port" '[.change.plan.servers[].steps[].rollout.routePorts.roll // empty] == [$port | tonumber]' >/dev/null ||
    fail "the plan of moving to port $port should say the service's step takes the route there: $(dw changes get "$(jq -r .id <<<"$change")" --json)"
  says name curl -fsS --max-time 5 --cacert "$pebble_dir/root.pem" --resolve "$roll_domain:443:127.0.0.1" "https://$roll_domain/who" ||
    fail "https://$roll_domain/ doesn't serve app after it moved to port $port"
  # The route reads as what its change said, which a route still on the old port wouldn't.
  [[ "$(drifted)" == "$drift_before" ]] || fail "drift after moving to port $port: $(dw drift list --json), and before the rollouts it was $drift_before"
}

rollout_step "The first deploy waits for the check, through the API"
start=$SECONDS
mapfile -t spec < <(app_spec slow 1)
dw services create app "${spec[@]}" --yes || fail "services create app failed"
((SECONDS - start >= slow_for)) || fail "the first deploy took $((SECONDS - start)) s, and the app takes $slow_for s to pass its check, so it didn't wait"
one_app "the first deploy" slow
first_app=$app
state=""
for _ in $(seq 60); do
  health=$(dw health list --service app --json) || fail "health list failed"
  state=$(jq -r '.services[0].state // empty' <<<"$health")
  [[ "$state" == HEALTH_STATE_HEALTHY ]] && break
  sleep 0.5
done
[[ "$state" == HEALTH_STATE_HEALTHY ]] || fail "the API says app is '$state' after 30 s, want healthy: $health"
jq -e '.services[0].servers[0].instances[0] | .check == "HEALTH_CHECK_KIND_HTTP" and .last.passed == true and (.last.output | contains("200"))' <<<"$health" >/dev/null ||
  fail "the API should say an HTTP check passed, answering 200, and says: $health"

rollout_step "A route, and its own certificate"
roll_domain=roll.e2e-dockwell.example.net
dw routes create roll --domain "$roll_domain" --service app --port 8080 --yes || fail "routes create roll failed"
[[ "$(app_containers)" == "$first_app "* ]] || fail "adding a route replaced the container, which is downtime: $(app_containers)"
serving=""
for _ in $(seq 120); do
  if curl -fsS --max-time 5 --cacert "$pebble_dir/root.pem" --resolve "$roll_domain:443:127.0.0.1" "https://$roll_domain/who" 2>/dev/null | grep -q name; then
    serving=1
    break
  fi
  sleep 0.5
done
[[ -n "$serving" ]] || fail "https://$roll_domain/ didn't serve app within 60 s"

rollout_step "Requests of every kind start, and go on until the end of this part"
# A rollout moves traffic by writing a file, so from here Caddy never reloads, and
# no other site on this server pays for it.
reloads() { docker logs dockwell-ingress 2>&1 | grep -c 'autosaved config' || true; }
reloads_before=$(reloads)
"$rollout_dir/e2e-load" drive --domain "$roll_domain" --addr 127.0.0.1:443 --roots "$pebble_dir/root.pem" --report "$rollout_dir/load.json" >>"$rollout_dir/load.log" 2>&1 &
load_pid=$!
sleep 3
kill -0 "$load_pid" 2>/dev/null || fail "the load stopped as it started: $(cat "$rollout_dir/load.log")"

rollout_step "A rolling deploy: the new one starts, turns healthy, and only then takes over"
start=$SECONDS
deploy_app slow 2 &
deploying=$!
both=""
for _ in $(seq 100); do
  [[ "$(app_containers | grep -c running)" == 2 ]] && both=1 && break
  sleep 0.1
done
[[ -n "$both" ]] || fail "two containers of app never ran at once, so this deploy didn't roll: $(app_containers)"
[[ "$(app_containers | grep "^$first_app ")" == "$first_app running "* ]] || fail "the old container isn't running beside the new one: $(app_containers)"
wait "$deploying" || fail "the rolling deploy failed"
((SECONDS - start >= slow_for)) || fail "the deploy took $((SECONDS - start)) s, and the app takes $slow_for s to pass its check, so it didn't wait"
one_app "after a rolling deploy" slow
[[ "$app" != "$first_app" ]] || fail "the deploy kept $app"
[[ "${app##*-}" -gt "${first_app##*-}" ]] || fail "the new container is $app and the old was $first_app, and a number only goes up"
app_env "$app" | grep -qx DEPLOY=2 || fail "$app's environment is $(app_env "$app")"
jq -e '.state == "CHANGE_STATE_APPLIED"' <<<"$(newest_change)" >/dev/null || fail "the deploy reads as: $(newest_change)"

rollout_step "An image that never turns healthy: it fails, rolls back, and says why"
kept=$app
kept_id=$(docker inspect --format '{{.Id}}' "$kept") || fail "docker inspect $kept failed"
count=$(changes_count) || fail "changes list failed"
if said=$(deploy_app never 3 2>&1); then fail "the CLI says a deploy that never turned healthy went through: $said"; fi
check_said="app runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable. Fix what it answers, or the check, then try again."
grep -qF "$check_said" <<<"$said" || fail "the CLI should say what the check answered, as '$check_said', and says: $said"
back=$(rolled_back)
one_app "after the rollback" slow
[[ "$(docker inspect --format '{{.Id}}' "$app")" == "$kept_id" ]] || fail "the rollback replaced what ran, $kept, with $app, and should have left it alone"
jq -e '.state == "CHANGE_STATE_APPLIED"' <<<"$back" >/dev/null || fail "the rollback should have applied, and is: $back"
jq -e --arg said "$check_said" '.because == $said' <<<"$back" >/dev/null || fail "the rollback should say why, as '$check_said', and says: $back"
said=$(dw changes get "$(jq -r .id <<<"$back")" 2>&1) || fail "changes get failed: $said"
grep -qF "Dockwell made this change because of what failed: $check_said" <<<"$said" || fail "the CLI's rollback should say why, as '$check_said', and says: $said"
failed=$(dw changes list --json | jq -c '.changes[1]')
jq -e --arg back "$(jq -r .id <<<"$back")" --arg said "$check_said" '.state == "CHANGE_STATE_FAILED" and .rolledBackBy == $back and .error == $said' <<<"$failed" >/dev/null ||
  fail "the failed deploy should stay on record as failed and rolled back, saying '$check_said', and is: $failed"
said=$(dw changes get "$(jq -r .id <<<"$failed")" 2>&1) || fail "changes get failed: $said"
grep -qF "It failed: $check_said" <<<"$said" || fail "the CLI's failed deploy should say '$check_said', and says: $said"
dw compose export >"$rollout_dir/after-rollback.yaml" || fail "compose export failed"
grep -q 'dockwell-e2e-app:slow' "$rollout_dir/after-rollback.yaml" || fail "the spec isn't back on the image that ran: $(cat "$rollout_dir/after-rollback.yaml")"
[[ "$(drifted)" == "$drift_before" ]] || fail "drift after a rollback: $(dw drift list --json), and before the rollouts it was $drift_before"

rollout_step "A rollback doesn't go round again"
sleep 8
[[ "$(changes_count)" == "$((count + 2))" ]] || fail "there were $count changes, and after a deploy and its rollback, and 8 s, there are $(changes_count), want $((count + 2))"
[[ "$(docker inspect --format '{{.Id}}' "$kept")" == "$kept_id" ]] || fail "$kept was replaced after the rollback"

rollout_step "A check Docker runs fails the same, with what it printed"
# The app prints a fixed sentence with a value from its environment inside it. The
# image set the sentence, and the spec set the value, so the words are quoted and
# the value is masked, as the reason is read by more people than the container is.
database=db.e2e-dockwell.internal
if deploy_app never 4 --app-env "E2E_DATABASE=$database" "${docker_check[@]}" >/dev/null 2>&1; then fail "the CLI says a deploy whose command check never passed went through"; fi
back=$(rolled_back)
one_app "after a command check's rollback" slow
[[ "$(docker inspect --format '{{.Id}}' "$app")" == "$kept_id" ]] || fail "the rollback from a command check replaced what ran, $kept, with $app"
check_printed="app runs, but fails its health check: the command exited with 1: \"e2e-load: 503: $never_says: •••\". Fix what it reports, or the check, then try again."
jq -e --arg said "$check_printed" '(.summary | startswith("Roll back")) and .because == $said' <<<"$back" >/dev/null ||
  fail "the rollback should quote what the command printed, as '$check_printed', and says: $back"
jq -e --arg value "$database" '.because | contains($value) | not' <<<"$back" >/dev/null ||
  fail "the rollback's reason shows a value from what the spec set: $back"

rollout_step "A deploy with the check Docker runs turns healthy by it, and a slow image waits"
start=$SECONDS
deploy_app slow 5 --app-healthcheck-test CMD --app-healthcheck-test /e2e-load --app-healthcheck-test check --app-healthcheck-interval 1s --app-healthcheck-timeout 3s --app-healthcheck-retries 3 --app-healthcheck-start-period 15s ||
  fail "a deploy with a command check failed"
((SECONDS - start >= slow_for)) || fail "the deploy took $((SECONDS - start)) s, and the app takes $slow_for s to pass its check, so it didn't wait"
one_app "after a deploy by Docker's check" slow
[[ "$(docker inspect --format '{{.State.Health.Status}}' "$app")" == healthy ]] || fail "Docker doesn't say $app is healthy: $(docker inspect --format '{{json .State.Health}}' "$app")"

rollout_step "A restart rolls, records itself, and leaves the spec alone"
before=$app
restart_output=$(dw services restart app --yes) || fail "services restart app failed: $restart_output"
one_app "after a restart" slow
[[ "$app" != "$before" ]] || fail "the restart left $app as it was"
app_env "$app" | grep -qx DEPLOY=5 || fail "the restart changed the spec: $(app_env "$app")"
restarted=$(newest_change) || fail "changes list failed"
jq -e '.restart == true and .state == "CHANGE_STATE_APPLIED"' <<<"$restarted" >/dev/null || fail "the restart reads as: $restarted"
if dw changes undo "$(jq -r .id <<<"$restarted")" --yes >/dev/null 2>&1; then fail "a restart was undone, with nothing to undo"; fi
plan=$(dw changes get "$(jq -r .id <<<"$restarted")" --json | jq -c '.change.plan.servers[0].steps[0]') || fail "changes get failed"
jq -e '.restart != "" and .restart != null and .rollout.rolling == true' <<<"$plan" >/dev/null || fail "the restart's plan should name itself and roll, and is: $plan"

rollout_step "The whole of Dockwell is killed mid-rollout, and it goes on when it is back"
before=$app
before_id=$(docker inspect --format '{{.Id}}' "$before") || fail "docker inspect $before failed"
deploy_app slow 6 "${http_check[@]}" >/dev/null 2>&1 &
deploying=$!
both=""
for _ in $(seq 100); do
  [[ "$(app_containers | grep -c running)" == 2 ]] && both=1 && break
  sleep 0.1
done
[[ -n "$both" ]] || fail "the deploy to kill never started a second container: $(app_containers)"
kill_dockwell
wait "$deploying" 2>/dev/null || true
echo "Killed with $(app_containers | grep -c running) of app running: $(app_containers | tr '\n' ';')"
[[ "$(docker inspect --format '{{.State.Running}}' "$before")" == true ]] || fail "$before stopped as Dockwell died"
start_dockwell
wait_for_dockwell
settled=""
for _ in $(seq 240); do
  if [[ "$(app_containers | wc -l)" == 1 ]] && [[ "$(newest_change | jq -r .state)" == CHANGE_STATE_APPLIED ]]; then
    settled=1
    break
  fi
  sleep 0.5
done
[[ -n "$settled" ]] || fail "120 s after Dockwell came back, the rollout hasn't ended in one container and an applied change"
one_app "after the kill" slow
app_env "$app" | grep -qx DEPLOY=6 || fail "after the kill, what runs is deploy $(app_env "$app" | grep DEPLOY), and the rollout should have gone on to 6"
[[ "$(docker inspect --format '{{.Id}}' "$app")" != "$before_id" ]] || fail "after the kill, what runs is the old container, and the change says it applied"
[[ "$(drifted)" == "$drift_before" ]] || fail "drift after a kill: $(dw drift list --json), and before the rollouts it was $drift_before"

rollout_step "A plain docker compose up reads what's there, and the next rollout reads what that leaves"
dw compose export >"$rollout_dir/compose.yaml" || fail "compose export failed"
GREETING=hello docker compose -f "$rollout_dir/compose.yaml" up --detach --no-recreate app ||
  fail "docker compose up on the export failed: $(cat "$rollout_dir/compose.yaml")"
before=$app
one_app "after docker compose up" slow
[[ "$app" == "$before" ]] || fail "docker compose up --no-recreate made $app of what was $before"
deploy_app slow 7 || fail "the rollout after docker compose up failed"
one_app "after the rollout after docker compose up" slow
[[ "${app##*-}" -gt "${before##*-}" ]] || fail "the rollout after docker compose up made $app, after $before"

rollout_step "An app and its route go to a new port in one change, and back, with the load still on"
moves_with_its_route 8081 71
moves_with_its_route 8080 7

rollout_step "What the load counted"
kill -TERM "$load_pid" || fail "the load had already stopped: $(cat "$rollout_dir/load.log")"
wait "$load_pid" || fail "the load ended badly: $(cat "$rollout_dir/load.log")"
load_pid=""
report=$(cat "$rollout_dir/load.json") || fail "the load wrote no report"
jq -r .summary <<<"$report"
echo "Served by $(jq '.servedBy | length' <<<"$report") containers: $(jq -c .servedBy <<<"$report")"
jq -e '.failed == 0' <<<"$report" >/dev/null || fail "requests failed during the rollouts. Read when each did against the times on the steps above: $(jq -c '.errors' <<<"$report")"
# Only a reload of Caddy cuts a new connection on its way in (ADR 0027), and nothing
# here reloads it.
[[ "$(reloads)" == "$reloads_before" ]] || fail "Caddy reloaded $(($(reloads) - reloads_before)) times during the rollouts, and a rollout should cost it none"
jq -e '.reset == 0' <<<"$report" >/dev/null || fail "$(jq .reset <<<"$report") new connections were cut on the way in, with no reload to cut them: $(jq -r .summary <<<"$report")"
for kind in short fresh browser post-http1 post-http2 slow stream websocket; do
  jq -e --arg kind "$kind" '.passed[$kind] >= 5' <<<"$report" >/dev/null || fail "only $(jq --arg kind "$kind" '.passed[$kind]' <<<"$report") of the $kind requests passed, too few to mean anything"
done
jq -e '.servedBy | length >= 5' <<<"$report" >/dev/null || fail "only $(jq '.servedBy | length' <<<"$report") containers served requests, so traffic didn't move: $(jq -c .servedBy <<<"$report")"

rollout_step "Stopping first, an app and its route go to a new port in one change, and back"
moves_with_its_route 8081 72 '{"drain":"10s","order":"ROLLOUT_ORDER_STOP_FIRST"}'
moves_with_its_route 8080 7

rollout_step "Killed while a stop-first deploy waits for its check: it still has to pass it, and it rolls back"
count=$(changes_count) || fail "changes list failed"
deploy_app never 8 "${http_check[@]}" --app-rollout-order stop-first >/dev/null 2>&1 &
deploying=$!
# The old one has gone, the new one has its start period to go, and nothing else of the
# app is left to say the step hasn't ended.
waiting=""
for _ in $(seq 300); do
  list=$(app_containers) || fail "docker ps failed"
  if [[ "$(wc -l <<<"$list")" == 1 && "$list" == *" running dockwell-e2e-app:never" ]]; then
    waiting=1
    break
  fi
  sleep 0.1
done
[[ -n "$waiting" ]] || fail "the stop-first deploy to kill never got as far as its check: $(app_containers)"
kill_dockwell
wait "$deploying" 2>/dev/null || true
start_dockwell
wait_for_dockwell
back=$(rolled_back)
jq -e '.state == "CHANGE_STATE_APPLIED"' <<<"$back" >/dev/null || fail "the rollback of a deploy killed as it waited should have applied, and is: $back"
failed=$(dw changes list --json | jq -c '.changes[1]')
jq -e --arg said "$check_said" '.state == "CHANGE_STATE_FAILED" and .error == $said' <<<"$failed" >/dev/null ||
  fail "a deploy killed as it waited for a check it never passes should read as failed, saying '$check_said', and is: $failed"
[[ "$(changes_count)" == "$((count + 2))" ]] || fail "there were $count changes, and after a deploy killed as it waited and its rollback, there are $(changes_count)"
one_app "after a deploy killed as it waited" slow
app_env "$app" | grep -qx DEPLOY=7 || fail "after the rollback, what runs is deploy $(app_env "$app" | grep DEPLOY), and should be the one before"

rollout_step "The app goes, leaving no container behind"
dw routes delete roll --yes || fail "routes delete roll failed"
dw services delete app --yes || fail "services delete app failed"
[[ -z "$(app_containers)" ]] || fail "containers are left after deleting the app: $(app_containers)"
in_rollout=""

step "Ingress: the last route stops Caddy and frees the ports, and its certificate stays"
certificate=$(priv find "$data/ingress/data/caddy/certificates" -name "$domain.crt") || fail "can't look for the certificate"
[[ -n "$certificate" ]] || fail "there's no certificate on disk for $domain"
before=$(priv openssl x509 -in "$certificate" -noout -serial) || fail "can't read the certificate"
dw routes delete www --yes || fail "routes delete www failed"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "dockwell-ingress is there after the last route"; fi
if docker network inspect dockwell_ingress >/dev/null 2>&1; then fail "the network dockwell_ingress is there after the last route"; fi
if docker inspect dockwell-ingress-keep >/dev/null 2>&1; then fail "dockwell-ingress-keep is there after the last route"; fi
if priv test -e "$data/ingress/last-good.json"; then fail "the agent's copy of Caddy's config is there after the last route"; fi
for p in 80 443; do
  if listening "$p"; then fail "port $p is still held after the last route"; fi
done
priv test -f "$certificate" || fail "the certificate went with Caddy"
priv test -d "$data/ingress/data/caddy/acme" || fail "the ACME account went with Caddy"
on=$(docker inspect --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$web") || fail "docker inspect $web failed"
if grep -q dockwell_ingress <<<"$on"; then fail "web stayed on the ingress network"; fi

step "Ingress: a route again reuses its certificate"
dw routes create www --domain "$domain" --service web --port 80 --yes || fail "routes create www, again, failed"
for _ in $(seq 100); do
  over_https >/dev/null 2>&1 && break
  sleep 0.1
done
says nginx over_https || fail "with the route back, https://$domain/ doesn't serve web"
after=$(priv openssl x509 -in "$certificate" -noout -serial) || fail "can't read the certificate"
[[ "$after" == "$before" ]] || fail "a new certificate was issued, $after, where the one kept was $before"
dw routes delete www --yes || fail "routes delete www, again, failed"

# The network prune above took the imported file's network, as nothing was on it by
# then, and it rightly stays as drift. So these routes are held to what they add.
drifted() { dw drift list --json | jq -c '[(.drift // [])[] | .resource] | sort_by(.kind, .name)'; }
drift_before=$(drifted) || fail "drift list failed"
[[ "$(jq -r 'map(.kind + " " + .name) | join(",")' <<<"$drift_before")" == "network back" ]] ||
  fail "before these routes, drift should be only the network the prune took, and is: $drift_before"

step "Ingress: a route that takes no certificate serves plain HTTP, on port 80 alone"
behind=behind.e2e-dockwell.example.net
# Each order Pebble takes, as it logs them. Nothing here may ask it for one.
orders() { grep -c 'Added order' "$pebble_dir/pebble.out" || true; }
orders_before=$(orders)
# An earlier step got a certificate from Pebble, or counting them proves nothing.
[[ "$orders_before" -gt 0 ]] || fail "Pebble has taken no order yet, so a count that doesn't move says nothing"
made=$(dw routes create behind --domain "$behind" --service web --port 80 --tls off --yes 2>&1) || fail "routes create behind failed: $made"
[[ "$made" == *"unencrypted"* ]] || fail "the plan doesn't say traffic reaches this server unencrypted: $made"
[[ "$made" == *"takes ports 80/tcp."* ]] || fail "the plan doesn't say Caddy takes port 80 alone: $made"
[[ "$made" == *"answers only what runs on"* ]] || fail "the plan doesn't say who it answers: $made"
listening 80 || fail "nothing listens on port 80 for a route that takes no certificate"
if listening 443; then fail "something listens on port 443 with no route needing it"; fi
[[ -z "$(ss -H -lun 'sport = :443')" ]] || fail "something listens on UDP 443 with no route needing it"
plain_http() { curl -sS --max-time 10 --resolve "$behind:80:127.0.0.1" "$@" "http://$behind/"; }
answer=$(plain_http -o /dev/null -w '%{http_code} %{redirect_url}') || fail "http://$behind/ didn't answer"
[[ "$answer" == "200 " ]] || fail "http://$behind/ answers $answer, want it served, as a redirect would loop behind a tunnel"
body=$(plain_http) || fail "http://$behind/ didn't answer"
[[ "$body" == *nginx* ]] || fail "http://$behind/ doesn't serve web: $body"
if curl -sS --max-time 3 -o /dev/null --resolve "$behind:443:127.0.0.1" "https://$behind/" 2>/dev/null; then fail "https://$behind/ answered, with nothing asked of it"; fi
state=""
for _ in $(seq 180); do
  listed=$(dw domains list --json 2>/dev/null) || listed='{}'
  state=$(jq -r --arg d "$behind" '.domains[]? | select(.name == $d) | .servers[0].status.certificate.state // empty' <<<"$listed")
  [[ "$state" == CERTIFICATE_STATE_NONE ]] && break
  sleep 0.5
done
[[ "$state" == CERTIFICATE_STATE_NONE ]] || fail "$behind's certificate reads '$state' after 90 s, want none"
message=$(jq -r --arg d "$behind" '.domains[] | select(.name == $d) | .servers[0].status.certificate.message' <<<"$listed")
[[ "$message" == *"TLS ends before this server"* ]] || fail "$behind's certificate says: $message"
url=$(jq -r --arg d "$behind" '.domains[] | select(.name == $d) | .url' <<<"$listed")
[[ "$url" == "http://$behind" ]] || fail "$behind answers at $url, as the API has it"
[[ "$(orders)" == "$orders_before" ]] || fail "a route that takes no certificate asked Pebble for one: $(grep 'Added order' "$pebble_dir/pebble.out")"
held=$(priv find "$data/ingress/data/caddy/certificates" -name "$behind.crt") || fail "can't look for a certificate"
[[ -z "$held" ]] || fail "there's a certificate for a route that takes none: $held"

step "Ingress: a route reaches what was taken on, with no restart"
routed_site=routed.e2e-dockwell.example.net
docker run --detach --name e2e-routed nginx:1.26-alpine >/dev/null || fail "docker run e2e-routed failed"
take_on e2e-routed routed
routed_before=$(touched e2e-routed) || fail "docker inspect e2e-routed failed"
made=$(dw routes create routed --domain "$routed_site" --service routed --port 80 --tls off --yes 2>&1) || fail "routes create routed failed: $made"
[[ "$(touched e2e-routed)" == "$routed_before" ]] || fail "a route touched e2e-routed: $routed_before, then $(touched e2e-routed)"
docker inspect --format '{{range $n, $e := .NetworkSettings.Networks}}{{println $n}}{{end}}' e2e-routed | grep -qx dockwell_ingress ||
  fail "e2e-routed isn't on the ingress network: $(docker inspect --format '{{json .NetworkSettings.Networks}}' e2e-routed)"
answer=""
for _ in $(seq 100); do
  answer=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' --resolve "$routed_site:80:127.0.0.1" "http://$routed_site/") && [[ "$answer" == 200 ]] && break
  sleep 0.2
done
[[ "$answer" == 200 ]] || fail "http://$routed_site/ answers $answer, want what was taken on to serve it"
dw routes delete routed --yes >/dev/null || fail "routes delete routed failed"
dw services delete routed --yes >/dev/null || fail "deleting routed failed"
[[ "$(touched e2e-routed)" == "$routed_before" ]] || fail "letting e2e-routed go touched it"
docker rm --force e2e-routed >/dev/null || fail "docker rm e2e-routed failed"

step "Ingress: a route with no certificate answers what it names, not the internet"
# This machine's own address stands for a stranger, as a request to it comes from it.
stranger=$(hostname -I | awk '{ print $1 }')
[[ -n "$stranger" && "$stranger" != 127.* ]] || fail "this machine has no address but loopback, so this proves nothing: '$stranger'"
straight() { curl -sS --max-time 10 -H "Host: $1" -o /dev/null -w '%{http_code}' "http://$stranger/"; }
nobody=$(curl -sS --max-time 10 -H 'Host: nobody.e2e-dockwell.example.net' "http://$stranger/") || fail "a name with no route didn't answer"
[[ "$nobody" == *"no site at this address"* ]] || fail "a name with no route says: $nobody"
reached=$(curl -sS --max-time 10 -H "Host: $behind" "http://$stranger/") || fail "http://$stranger/ didn't answer"
[[ "$reached" == "$nobody" ]] || fail "a stranger at this server's own address got this from a route behind a tunnel, not what a name with no route gets: $reached"

# Each way docs/install.md says to run something like cloudflared, with a container
# of nginx's image doing what it would. wget says the status when it fails, so a
# timeout doesn't read as a refusal.
from_host=$(docker run --rm --network host nginx:1.27-alpine wget -q -T 5 -O- --header "Host: $behind" http://127.0.0.1:80/) ||
  fail "a container on the server's own network can't reach Caddy at 127.0.0.1:80"
[[ "$from_host" == *nginx* ]] || fail "from the server's own network, 127.0.0.1:80 answers: $from_host"
on_network() { docker run --rm --network "$1" --add-host host.docker.internal:host-gateway nginx:1.27-alpine sh -c "wget -T 5 -O- --header 'Host: $2' http://host.docker.internal:80/ 2>&1" || true; }
docker network create -o com.docker.network.bridge.name=e2e-tunnel0 e2e-tunnel >/dev/null || fail "docker network create e2e-tunnel failed"
from_tunnel=$(on_network e2e-tunnel "$behind")
[[ "$from_tunnel" == *"404"* ]] || fail "a container on a network of its own, with its range not named, got: $from_tunnel"
tunnel_range=$(docker network inspect --format '{{(index .IPAM.Config 0).Subnet}}' e2e-tunnel) || fail "docker network inspect e2e-tunnel failed"
bridged=bridged.e2e-dockwell.example.net
set +e
planned=$(dw routes create bridged --domain "$bridged" --service web --port 80 --tls off --trusted-proxies "$tunnel_range" --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "a route naming a proxy exited $status, want 4: it waits for a person: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
person_apply "$(jq -r '.changes[0].id' <<<"$changes")"
from_tunnel=$(on_network e2e-tunnel "$bridged")
[[ "$from_tunnel" == *nginx* ]] || fail "a container on a network of its own, with its range named, got: $from_tunnel"
# Docker's default network is where a service with no networks runs, and a token can
# deploy one, so what's there must not be what this route answers.
from_default=$(on_network bridge "$bridged")
[[ "$from_default" == *"404"* ]] || fail "a container on Docker's default network reached a route naming another network: $from_default"
dw routes delete bridged --yes || fail "routes delete bridged failed"
docker network rm e2e-tunnel >/dev/null || fail "docker network rm e2e-tunnel failed"

# Both sides of why the guide gives a proxy a network of its own: a service with no
# networks, which a token can deploy, runs on Docker's default network, so a route
# naming that range answers it and believes what it says. web is such a service. A
# container on any other network is a stranger to that route.
[[ "$(docker inspect --format '{{if index .NetworkSettings.Networks "bridge"}}yes{{end}}' "$web")" == yes ]] ||
  fail "web, a service with no networks, isn't on Docker's default network: $(docker inspect --format '{{json .NetworkSettings.Networks}}' "$web")"
bridge_range=$(docker network inspect --format '{{(index .IPAM.Config 0).Subnet}}' bridge) || fail "docker network inspect bridge failed"
bridge_gateway=$(docker network inspect --format '{{(index .IPAM.Config 0).Gateway}}' bridge) || fail "docker network inspect bridge failed"
on_bridge=onbridge.e2e-dockwell.example.net
set +e
planned=$(dw routes create onbridge --domain "$on_bridge" --service web --port 80 --tls off --trusted-proxies "$bridge_range" --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "a route naming Docker's default network exited $status, want 4: it waits for a person: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
person_apply "$(jq -r '.changes[0].id' <<<"$changes")"
from_service=$(docker exec "$web" wget -q -T 5 -O- --header "Host: $on_bridge" --header 'X-Forwarded-For: 203.0.113.9' "http://$bridge_gateway:80/" 2>&1) ||
  fail "web, a service on Docker's default network, can't reach a route naming that range: $from_service"
[[ "$from_service" == *nginx* ]] || fail "web, a service on Docker's default network, got this from a route naming that range: $from_service"
sleep 0.5
seen=$(docker logs --tail 5 "$web" 2>&1) || fail "docker logs $web failed"
seen=${seen##*$'\n'}
[[ "$seen" == *'"203.0.113.9"' ]] || fail "a route naming Docker's default network didn't believe a service there on its client: $seen"
docker network create e2e-other >/dev/null || fail "docker network create e2e-other failed"
from_other=$(on_network e2e-other "$on_bridge")
[[ "$from_other" == *"404"* ]] || fail "a container on another network reached a route naming Docker's default network: $from_other"
dw routes delete onbridge --yes || fail "routes delete onbridge failed"
docker network rm e2e-other >/dev/null || fail "docker network rm e2e-other failed"

# Anyone takes a person, and says so.
open=open.e2e-dockwell.example.net
set +e
planned=$(dw routes create open --domain "$open" --service web --port 80 --tls off --plain-for-anyone --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "a route for anyone exited $status, want 4: it waits for a person: $planned"
[[ "$planned" == *"answers anyone who can reach"* ]] || fail "the plan doesn't say it answers anyone: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
anyone=$(jq -r '.changes[0].id' <<<"$changes")
token_refused "$anyone"
person_apply "$anyone"
[[ "$(straight "$open")" == 200 ]] || fail "a route for anyone doesn't answer at $stranger"
dw routes delete open --yes || fail "routes delete open failed"

step "Ingress: a route to a port on the server needs a person, names the port, and believes the proxy it names"
on_server=$((web_port + 1))
docker run -d --name e2e-on-server -p "127.0.0.1:$on_server:80" nginx:1.27-alpine >/dev/null || fail "can't run something on port $on_server"
for _ in $(seq 100); do
  curl -fsS --max-time 1 -o /dev/null "http://127.0.0.1:$on_server/" 2>/dev/null && break
  sleep 0.1
done
curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:$on_server/" || fail "nothing answers on port $on_server, so this proves nothing"
through=through.e2e-dockwell.example.net
set +e
planned=$(dw routes create through --domain "$through" --host-port "$on_server" --tls off --trusted-proxies 127.0.0.1 --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "a route to a port on the server exited $status, want 4: it waits for a person: $planned"
[[ "$planned" == *"127.0.0.1:$on_server"* ]] || fail "the plan doesn't name the port: $planned"
[[ "$planned" == *"127.0.0.1/32 as a proxy"* ]] || fail "the plan doesn't name the proxy: $planned"
[[ "$planned" == *"sees each caller as this server"* ]] || fail "the plan doesn't say what's there sees everyone as local: $planned"
[[ "$planned" == *"the container e2e-on-server"* ]] || fail "the plan doesn't name what listens there: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
reach=$(jq -r '.changes[0].id' <<<"$changes")
[[ "$reach" == chg_* ]] || fail "changes list has no change: $changes"
# The CLI stops short of asking, so this is the server's own refusal.
token_refused "$reach"
person_apply "$reach"
answer=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' --resolve "$through:80:127.0.0.1" -H 'X-Forwarded-For: 6.6.6.6, 198.51.100.7' "http://$through/") ||
  fail "http://$through/ didn't answer"
[[ "$answer" == 200 ]] || fail "http://$through/ answers $answer"
sleep 0.5
seen=$(docker logs --tail 5 e2e-on-server 2>&1) || fail "docker logs e2e-on-server failed"
seen=${seen##*$'\n'}
[[ "$seen" == *'"198.51.100.7"' ]] || fail "what's on the server doesn't read the address its proxy found, in X-Forwarded-For: $seen"
if [[ "$seen" == *6.6.6.6* ]]; then fail "what a client sent itself reached what's on the server: $seen"; fi
# And the same request, on the route that names no proxy, believes none of it.
answer=$(plain_http -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 6.6.6.6') || fail "http://$behind/ didn't answer"
[[ "$answer" == 200 ]] || fail "http://$behind/ answers $answer"
sleep 0.5
seen=$(docker logs --tail 5 "$web" 2>&1) || fail "docker logs $web failed"
seen=${seen##*$'\n'}
[[ "$seen" == *'"127.0.0.1"' ]] || fail "beside a route that trusts a proxy, one that doesn't reads: $seen"
[[ "$(drifted)" == "$drift_before" ]] || fail "drift after routes behind a tunnel: $(dw drift list --json), and before them it was $drift_before"
set +e
own=$(dw routes create own --domain own.e2e-dockwell.example.net --host-port "$port" --tls off --yes 2>&1)
status=$?
set -e
[[ "$status" != 0 ]] || fail "a route went through to the panel's own port"
[[ "$own" == *"one Dockwell listens on itself"* ]] || fail "the refusal of the panel's own port says: $own"
set +e
own=$(dw routes create own --domain own.e2e-dockwell.example.net --host-port 2375 --tls off --yes 2>&1)
status=$?
set -e
[[ "$status" != 0 ]] || fail "a route went through to Docker's API"
[[ "$own" == *"Docker's API"* ]] || fail "the refusal of Docker's API says: $own"

step "Websites: one sends to a URL through a web server of its own, moves to a port a person approves, and goes"
# What a website sends to: something elsewhere, which a container published on the
# server's own address stands for, as the website's web server is on a network of its
# own and reaches no other bridge, and something on the server's own port.
url_port=$((web_port + 5))
docker run -d --name e2e-site-url -p "$url_port:80" nginx:1.27-alpine sh -c 'echo from-the-url >/usr/share/nginx/html/index.html && exec nginx -g "daemon off;"' >/dev/null ||
  fail "can't run what the website sends to"
upstream=$(ip -4 route get 192.0.2.1 | awk '{ for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1) }')
[[ -n "$upstream" ]] || fail "this server has no address of its own to send to: $(ip -4 route get 192.0.2.1)"
upstream="$upstream:$url_port"
site_port=$((web_port + 4))
docker run -d --name e2e-site-port -p "127.0.0.1:$site_port:80" nginx:1.27-alpine sh -c 'echo from-the-port >/usr/share/nginx/html/index.html && exec nginx -g "daemon off;"' >/dev/null ||
  fail "can't run something on port $site_port"
site=site.e2e-dockwell.example.net
site_says() {
  local body=""
  for _ in $(seq 150); do
    body=$(curl -sS --max-time 5 --resolve "$site:80:127.0.0.1" "http://$site/" 2>/dev/null) && [[ "$body" == *"$1"* ]] && return 0
    sleep 0.2
  done
  fail "http://$site/ says '$body', want $1"
}
# A URL reaches what any container reaches, so a token's plan is a token's to apply.
made=$(dw websites create --domain "$site" --website "{\"proxy\":{\"url\":\"http://$upstream/\"}}" --tls off --yes 2>&1) ||
  fail "websites create failed: $made"
listed=$(dw websites list --json) || fail "websites list failed"
[[ "$(jq -r --arg d "$site" '.websites[] | select(.domain == $d) | .serviceId' <<<"$listed")" == svc_* ]] ||
  fail "the website has no web server: $listed"
site_says from-the-url
site_web=$(docker ps --filter label=com.docker.compose.service=web --format '{{.Names}} {{.Image}}' | awk '$2 ~ /^caddy:/ { print $1 }')
[[ -n "$site_web" && "$site_web" != *$'\n'* ]] || fail "Docker runs '$site_web' for the website's web server, want one: $(docker ps --format '{{.Names}} {{.Image}}')"
# Caddy's binary carries NET_BIND_SERVICE as a file capability, which it can't start
# without, so that one stays, and it runs as nobody's user.
runs_as=$(docker inspect --format '{{.Config.User}} {{json .HostConfig.CapDrop}}' "$site_web") || fail "docker inspect $site_web failed"
[[ "$runs_as" == "65532 "*NET_RAW* && "$runs_as" == *SETUID* && "$runs_as" != *NET_BIND_SERVICE* ]] ||
  fail "the website's web server runs as $runs_as"
# It's on a network of its own and the ingress's, never Docker's default bridge, whose
# containers could say who a visitor is, and it may use no more than 128 MB.
on=$(docker inspect --format '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}} {{end}}{{.HostConfig.Memory}}' "$site_web") ||
  fail "docker inspect $site_web failed"
[[ "$on" == *_site\ * && "$on" == *dockwell_ingress* && "$on" != *"bridge "* && "$on" == *" 134217728" ]] ||
  fail "the website's web server joins and may use $on, want its own network, the ingress's and 128 MB"
# The visitor's address goes on to what it sends to, past the ingress and the web server.
curl -sS --max-time 5 -o /dev/null --resolve "$site:80:127.0.0.1" "http://$site/" || fail "http://$site/ didn't answer"
sleep 0.5
seen=$(docker logs --tail 1 e2e-site-url 2>&1) || fail "docker logs e2e-site-url failed"
[[ "$seen" == *'"127.0.0.1, '* ]] || fail "what the website sends to doesn't read the visitor in X-Forwarded-For: $seen"
# A port on the server sees every visitor as local, so only a person applies it.
set +e
planned=$(dw websites update "$site" --website "{\"proxy\":{\"port\":$site_port}}" --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "a website to a port on the server exited $status, want 4: it waits for a person: $planned"
[[ "$planned" == *"127.0.0.1:$site_port"* ]] || fail "the plan doesn't name the port: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
moving=$(jq -r '.changes[0].id' <<<"$changes")
token_refused "$moving"
person_apply "$moving"
site_says from-the-port
# Its server says whether anything listens there, apart from deploys.
reach=""
for _ in $(seq 60); do
  reach=$(dw websites list --json | jq -c --arg d "$site" '.websites[] | select(.domain == $d) | .reach // {}')
  [[ "$reach" == *'"answered":true'* ]] && break
  sleep 1
done
[[ "$reach" == *'"answered":true'* && "$reach" == *"listens on port $site_port"* ]] || fail "the website's port reads $reach, want something listening"
docker stop e2e-site-port >/dev/null || fail "can't stop what listens on port $site_port"
for _ in $(seq 60); do
  reach=$(dw websites list --json | jq -c --arg d "$site" '.websites[] | select(.domain == $d) | .reach // {}')
  [[ "$reach" == *"nothing listens on port $site_port"* ]] && break
  sleep 1
done
[[ "$reach" == *"nothing listens on port $site_port"* && "$reach" != *'"answered":true'* ]] ||
  fail "with nothing on port $site_port, the website reads $reach"
docker start e2e-site-port >/dev/null || fail "can't start what listens on port $site_port again"
for _ in $(seq 100); do
  docker inspect "$site_web" >/dev/null 2>&1 || break
  sleep 0.1
done
if docker inspect "$site_web" >/dev/null 2>&1; then fail "the web server stayed once the website sent to a port"; fi
# Deleting it takes its route, and the name answers as one with no route.
gone=$(dw websites delete "$site" --yes 2>&1) || fail "websites delete failed: $gone"
answer=""
for _ in $(seq 100); do
  answer=$(curl -sS --max-time 5 --resolve "$site:80:127.0.0.1" "http://$site/" 2>/dev/null) && [[ "$answer" == *"no site at this address"* ]] && break
  sleep 0.1
done
[[ "$answer" == *"no site at this address"* ]] || fail "a deleted website still answers: $answer"
[[ "$(dw websites list --json | jq '.websites // [] | length')" == 0 ]] || fail "websites list still has it: $(dw websites list --json)"
[[ "$(drifted)" == "$drift_before" ]] || fail "drift after a website: $(dw drift list --json), and before it was $drift_before"
docker rm --force e2e-site-url e2e-site-port >/dev/null || fail "can't remove what the website sent to"

step "Websites: one serves a folder a person approves, and an edit through Files shows with nothing to deploy"
# Its web server reads the folder as nobody's user, so the folder and its files are open to everyone.
site_root=$(mktemp -d) || fail "can't make a folder for the website"
if ! { chmod 755 "$site_root" && printf 'from-the-folder\n' >"$site_root/index.html" && chmod 644 "$site_root/index.html"; }; then
  fail "can't write the website's folder"
fi
folder_site=folder.e2e-dockwell.example.net
folder_says() {
  local body=""
  for _ in $(seq 150); do
    body=$(curl -sS --max-time 5 --resolve "$folder_site:80:127.0.0.1" "http://$folder_site/" 2>/dev/null) && [[ "$body" == *"$1"* ]] && return 0
    sleep 0.2
  done
  fail "http://$folder_site/ says '$body', want $1"
}
set +e
planned=$(dw websites create --domain "$folder_site" --website "{\"folder\":{\"root\":\"$site_root\"}}" --tls off --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "a website that serves a folder exited $status, want 4: it waits for a person: $planned"
[[ "$planned" == *"$site_root"* ]] || fail "the plan doesn't name the folder: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
serving=$(jq -r '.changes[0].id' <<<"$changes")
token_refused "$serving"
person_apply "$serving"
folder_says from-the-folder
missing=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' --resolve "$folder_site:80:127.0.0.1" "http://$folder_site/nothing-here") ||
  fail "http://$folder_site/nothing-here didn't answer"
[[ "$missing" == 404 ]] || fail "a path the folder hasn't got answers $missing, want 404"
folder_web=$(docker ps --filter label=com.docker.compose.service=web --format '{{.Names}} {{.Image}}' | awk '$2 ~ /^caddy:/ { print $1 }')
[[ -n "$folder_web" && "$folder_web" != *$'\n'* ]] || fail "Docker runs '$folder_web' for the folder's web server, want one"
mounted=$(docker inspect --format '{{range .Mounts}}{{.Source}}:{{.Destination}}:{{.RW}} {{end}}' "$folder_web") || fail "docker inspect $folder_web failed"
[[ "$mounted" == *"$site_root:/srv:false"* ]] || fail "the folder's web server mounts $mounted, want $site_root at /srv, read-only"
# A person edits the file in Files, and the next request has it, with nothing deployed.
server_id=$(dw servers list --json | jq -r '.servers[0].id') || fail "servers list failed"
opened=$(person_call FileService/ReadFile "$(jq -nc --arg s "$server_id" --arg p "$site_root/index.html" '{serverId: $s, path: $p}')") ||
  fail "a person can't read the website's index"
version=$(jq -r '.version' <<<"$opened")
person_call FileService/SaveFile "$(jq -nc --arg s "$server_id" --arg p "$site_root/index.html" --arg v "$version" '{serverId: $s, path: $p, content: "edited-in-files\n", version: $v}')" >/dev/null ||
  fail "a person can't save the website's index"
folder_says edited-in-files
# Deleting the website leaves its folder as it was.
gone=$(dw websites delete "$folder_site" --yes 2>&1) || fail "websites delete failed: $gone"
[[ "$(cat "$site_root/index.html")" == edited-in-files ]] || fail "deleting the website touched its folder: $(ls -la "$site_root")"
[[ "$(drifted)" == "$drift_before" ]] || fail "drift after a folder's website: $(dw drift list --json), and before it was $drift_before"
rm -rf "$site_root"

step "Ingress: a route with a certificate takes port 443 beside them, and frees it as it goes"
took=$(dw routes create www --domain "$domain" --service web --port 80 --yes 2>&1) || fail "routes create www failed: $took"
[[ "$took" == *"takes ports 443/tcp and 443/udp too"* ]] || fail "the plan doesn't say Caddy takes port 443: $took"
for p in 80 443; do
  listening "$p" || fail "nothing listens on port $p with routes of both kinds"
done
for _ in $(seq 300); do
  over_https >/dev/null 2>&1 && break
  sleep 0.1
done
body=$(over_https) || fail "https://$domain/ doesn't answer beside a plain route"
[[ "$body" == *nginx* ]] || fail "https://$domain/ doesn't serve web: $body"
redirect=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' --resolve "$domain:80:127.0.0.1" "http://$domain/") || fail "http://$domain/ didn't answer"
[[ "$redirect" == 308 ]] || fail "http://$domain/ answers $redirect, want its redirect"
answer=$(plain_http -o /dev/null -w '%{http_code}') || fail "http://$behind/ didn't answer"
[[ "$answer" == 200 ]] || fail "beside a route with a certificate, http://$behind/ answers $answer"
# Taking it off HTTPS is a person's, and says what stops.
set +e
planned=$(dw routes update www --domain "$domain" --service web --port 80 --tls off --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "taking a live domain off HTTPS exited $status, want 4: it waits for a person: $planned"
[[ "$planned" == *"which stops"* && "$planned" == *HSTS* ]] || fail "the plan doesn't say HTTPS stops: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
taking=$(jq -r '.changes[0].id' <<<"$changes")
token_refused "$taking"
dw changes discard "$taking" >/dev/null || fail "changes discard failed"
freed=$(dw routes delete www --yes 2>&1) || fail "routes delete www failed: $freed"
[[ "$freed" == *"frees ports 443/tcp and 443/udp"* ]] || fail "the plan doesn't say Caddy frees port 443: $freed"
if listening 443; then fail "port 443 is still held with no route needing it"; fi
answer=$(plain_http -o /dev/null -w '%{http_code}') || fail "http://$behind/ didn't answer"
[[ "$answer" == 200 ]] || fail "after port 443 was freed, http://$behind/ answers $answer"

# The script removes what it was installed as, so it runs only where nothing is.
if [[ -n "${E2E_ROOT:-}" && ! -e /usr/local/bin/dockwell ]]; then
  step "Ingress: uninstalling says what stays, and frees the ports when asked"
  install_script=$(dirname "$0")/install.sh
  stop_dockwell
  said=$(sudo -n sh "$install_script" --uninstall 2>&1) || fail "uninstalling failed: $said"
  [[ "$said" == *"--uninstall --remove-ingress"* ]] || fail "uninstalling doesn't say how to free the ports: $said"
  [[ "$said" == *"before you delete"* ]] || fail "uninstalling doesn't say to do that before deleting the data: $said"
  [[ "$(docker inspect --format '{{.State.Running}}' dockwell-ingress)" == true ]] || fail "uninstalling stopped Caddy, which it wasn't asked to"
  answer=$(plain_http -o /dev/null -w '%{http_code}') || fail "http://$behind/ didn't answer"
  [[ "$answer" == 200 ]] || fail "uninstalling stopped a route answering: $answer"
  # A stopped service comes off the network too, or it couldn't start again.
  docker stop --time 1 "$web" >/dev/null || fail "can't stop web"
  said=$(sudo -n sh "$install_script" --uninstall --remove-ingress 2>&1) || fail "removing the ingress failed: $said"
  [[ "$said" == *"the ports it held are free"* ]] || fail "removing the ingress says: $said"
  if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "dockwell-ingress is there after removing the ingress"; fi
  if docker inspect dockwell-ingress-keep >/dev/null 2>&1; then fail "dockwell-ingress-keep is there after removing the ingress"; fi
  if docker network inspect dockwell_ingress >/dev/null 2>&1; then fail "the network dockwell_ingress is there after removing the ingress"; fi
  if listening 80; then fail "port 80 is still held after removing the ingress"; fi
  docker start "$web" >/dev/null || fail "a stopped service can't start after removing the ingress: it still names the network"
  [[ "$(docker inspect --format '{{.State.Running}}' e2e-on-server)" == true ]] || fail "removing the ingress stopped what isn't its own"

  step "Ingress: installing again with its data takes the ports back for the routes"
  start_dockwell
  wait_for_dockwell
  answer=""
  for _ in $(seq 300); do
    answer=$(plain_http -o /dev/null -w '%{http_code}' 2>/dev/null) || answer=""
    [[ "$answer" == 200 ]] && break
    sleep 0.2
  done
  [[ "$answer" == 200 ]] || fail "with Dockwell back, http://$behind/ answers '$answer' after 60 s"
else
  echo "Uninstalling wasn't run on real Docker, as that needs E2E_ROOT, and no Dockwell installed here. CI has both"
fi

dw routes delete through --yes || fail "routes delete through failed"
docker rm --force e2e-on-server >/dev/null || fail "can't remove e2e-on-server"
gone=$(dw routes delete behind --yes 2>&1) || fail "routes delete behind failed: $gone"
[[ "$gone" == *"so Caddy stops, and ports 80/tcp are free"* ]] || fail "the last plain route doesn't say what it frees: $gone"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "dockwell-ingress is there after the last route"; fi
if listening 80; then fail "port 80 is still held after the last route"; fi

# Moby takes a container's own no-new-privileges over the daemon's. It needs root,
# and a Docker that systemd runs, to restart it with the setting.
if [[ -n "${E2E_ROOT:-}" ]] && systemctl is-active --quiet docker 2>/dev/null; then
  step "Ingress: a Docker with no-new-privileges runs Caddy, and nothing else changes"
  daemon_before=$(priv cat /etc/docker/daemon.json 2>/dev/null) || daemon_before=""
  daemon_changed=1
  jq '. + {"no-new-privileges": true}' <<<"${daemon_before:-"{}"}" | priv tee /etc/docker/daemon.json >/dev/null || fail "can't change /etc/docker/daemon.json"
  priv systemctl restart docker || fail "can't restart Docker with no-new-privileges"
  options=$(docker info --format '{{json .SecurityOptions}}') || fail "Docker didn't come back with no-new-privileges"
  [[ "$options" == *name=no-new-privileges* ]] || fail "Docker doesn't say it sets no-new-privileges, so this proves nothing: $options"
  other=$(docker run --rm nginx:1.27-alpine grep NoNewPrivs /proc/self/status) || fail "can't run a container under no-new-privileges"
  [[ "$other" =~ NoNewPrivs:[[:space:]]+1 ]] || fail "a container on this daemon has $other, so this proves nothing"
  # The agent may not have seen Docker come back yet.
  started=""
  for _ in $(seq 30); do
    if made=$(dw routes create www --domain "$domain" --service web --port 80 --yes 2>&1); then
      started=1
      break
    fi
    sleep 2
  done
  [[ -n "$started" ]] || fail "on a Docker with no-new-privileges, a route failed: $made"
  [[ "$(docker inspect --format '{{json .HostConfig.SecurityOpt}}' dockwell-ingress)" == *no-new-privileges=false* ]] || fail "Caddy's container doesn't say no-new-privileges=false"
  caddy_says=$(docker exec dockwell-ingress grep NoNewPrivs /proc/1/status) || fail "can't ask Caddy's container"
  [[ "$caddy_says" =~ NoNewPrivs:[[:space:]]+0 ]] || fail "Caddy runs with $caddy_says, and would ignore its file capability"
  for _ in $(seq 100); do
    keeper=$(docker inspect --format '{{.State.Running}}' dockwell-ingress-keep 2>/dev/null) || keeper=""
    [[ "$keeper" == true ]] && break
    sleep 0.1
  done
  keeper_says=$(docker exec dockwell-ingress-keep grep NoNewPrivs /proc/1/status) || fail "can't ask the keeper"
  [[ "$keeper_says" =~ NoNewPrivs:[[:space:]]+1 ]] || fail "the keeper runs with $keeper_says, and needs nothing that takes off"
  for p in 80 443; do
    listening "$p" || fail "Caddy can't bind port $p on a Docker with no-new-privileges: $(docker logs --tail 20 dockwell-ingress 2>&1)"
  done
  for _ in $(seq 300); do
    over_https >/dev/null 2>&1 && break
    sleep 0.1
  done
  body=$(over_https) || fail "on a Docker with no-new-privileges, https://$domain/ doesn't answer"
  [[ "$body" == *nginx* ]] || fail "on a Docker with no-new-privileges, https://$domain/ doesn't serve web: $body"
  restore_daemon || fail "can't put Docker's settings back"
  removed=""
  for _ in $(seq 30); do
    if made=$(dw routes delete www --yes 2>&1); then
      removed=1
      break
    fi
    sleep 2
  done
  [[ -n "$removed" ]] || fail "routes delete www failed with Docker's settings back: $made"
else
  echo "Docker wasn't run with no-new-privileges, as that needs E2E_ROOT and systemd. CI has both"
fi

if [[ -n "${E2E_PANEL:-}" ]]; then
  step "Ingress: the panel's own domain, on its own ports"
  panel=panel.e2e-dockwell.example.net
  panel_url() { curl -fsS --max-time 10 --cacert "$pebble_dir/root.pem" --resolve "$panel:443:127.0.0.1" "$@"; }
  wait_for_panel() {
    for _ in $(seq 120); do
      [[ "$(panel_url "https://$panel/healthz" 2>/dev/null)" == ok ]] && return 0
      sleep 0.5
    done
    return 1
  }
  plain_status() { curl -sS --max-time 5 -o /dev/null -w '%{http_code}' --resolve "$panel:80:127.0.0.1" "$@" "http://$panel/"; }
  if [[ -z "${E2E_ROOT:-}" ]]; then
    sudo -n sysctl -q -w net.ipv4.ip_unprivileged_port_start=80 || fail "can't let this user bind ports 80 and 443, which the panel needs to hold them itself"
  fi
  stop_dockwell
  start_dockwell --domain "$panel"
  wait_for_dockwell
  wait_for_panel || fail "https://$panel/healthz didn't answer within 60 s, on the panel's own certificate"
  [[ "$(plain_status -H 'Authorization: Bearer x')" == 400 ]] || fail "a token over plain HTTP wasn't refused by the panel itself"
  if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress ran with no route"; fi

  step "Ingress: the first route moves the panel's domain behind Caddy, which only a person may do"
  set +e
  moved=$(dw routes create www --domain "$domain" --service web --port 80 --yes 2>&1)
  status=$?
  set -e
  [[ "$status" == 4 ]] || fail "moving the panel's domain behind Caddy exited $status, want 4, as a person approves it: $moved"
  grep -q "moves behind Caddy" <<<"$moved" || fail "the plan doesn't say the panel moves: $moved"
  grep -q "moves the panel's domain behind Caddy, so it needs a person" <<<"$moved" || fail "the CLI doesn't say why it needs a person: $moved"
  if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress started for a plan that waits for a person"; fi
  changes=$(dw changes list --json) || fail "changes list failed"
  move=$(jq -r '.changes[0].id' <<<"$changes")
  token_refused "$move"
  wait_for_panel || fail "https://$panel/healthz stopped answering on the panel's own certificate while the plan waited"
  person_apply "$move"
  says true docker inspect --format '{{.State.Running}}' dockwell-ingress || fail "dockwell-ingress isn't running"
  wait_for_panel || fail "https://$panel/healthz didn't answer through Caddy within 60 s"
  says nginx over_https || fail "with the panel beside it, https://$domain/ doesn't serve web"
  state=$(panel_url -X POST -H 'Content-Type: application/json' -d '{}' "https://$panel/dockwell.v1.AuthService/GetAuthState") ||
    fail "the panel's API doesn't answer through Caddy"
  [[ "$(jq -r .rpId <<<"$state")" == "$panel" ]] || fail "through Caddy, the panel's RP ID is not its domain: $state"
  [[ "$(plain_status -H 'Authorization: Bearer x')" == 400 ]] || fail "through Caddy, a token over plain HTTP wasn't refused"
  [[ "$(plain_status -X POST)" == 400 ]] || fail "through Caddy, a POST over plain HTTP wasn't refused"
  [[ "$(plain_status)" == 308 ]] || fail "through Caddy, a GET over plain HTTP wasn't redirected"
  [[ "$(priv stat -c '%a' "$data/ingress/run/panel.sock")" == 600 ]] || fail "the panel's socket has mode $(priv stat -c '%a' "$data/ingress/run/panel.sock"), want 600"
  [[ "$(priv stat -c '%u' "$data/ingress/run/panel.sock")" == "$caddy_uid" ]] || fail "the panel's socket is owned by uid $(priv stat -c '%u' "$data/ingress/run/panel.sock"), want $caddy_uid"

  step "Ingress: nothing on loopback can pretend to be HTTPS"
  spoofed=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' -H "Host: $panel" -H 'X-Forwarded-Proto: https' -H 'X-Forwarded-For: 203.0.113.9' "http://$addr/healthz")
  [[ "$spoofed" == 421 ]] || fail "the panel's Host on loopback, with forwarded headers, got $spoofed, want 421"

  step "Ingress: with Caddy down, the tunnel's listener still answers"
  docker stop --time 5 dockwell-ingress >/dev/null || fail "docker stop dockwell-ingress failed"
  [[ "$(curl -fsS --max-time 5 "http://$addr/healthz")" == ok ]] || fail "with Caddy down, the panel's loopback listener doesn't answer"
  # The agent starts it too, within a minute, so it may have done so already.
  docker start dockwell-ingress >/dev/null 2>&1 || true
  wait_for_panel || fail "https://$panel/healthz didn't come back with Caddy"

  step "Ingress: a restart of Dockwell leaves the ports to Caddy"
  stop_dockwell
  start_dockwell --domain "$panel"
  wait_for_dockwell
  grep -q 'hold ports 80 and 443' "$log" || fail "Dockwell didn't say the ports are Caddy's"
  wait_for_panel || fail "after a restart of Dockwell, https://$panel/healthz doesn't answer"

  step "Ingress: the last route gives the panel its ports back"
  back=$(dw routes delete www --yes 2>&1) || fail "routes delete www beside the panel failed: $back"
  grep -q "takes those ports back" <<<"$back" || fail "the plan doesn't say the panel takes its ports back: $back"
  if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "dockwell-ingress is there after the last route"; fi
  wait_for_panel || fail "https://$panel/healthz didn't answer on the panel's own ports again within 60 s"
  state=$(panel_url -X POST -H 'Content-Type: application/json' -d '{}' "https://$panel/dockwell.v1.AuthService/GetAuthState") ||
    fail "the panel's API doesn't answer on its own ports"
  [[ "$(jq -r .rpId <<<"$state")" == "$panel" ]] || fail "back on its own ports, the panel's RP ID is not its domain: $state"
  stop_dockwell
  if [[ -z "${E2E_ROOT:-}" ]]; then
    sudo -n sysctl -q -w net.ipv4.ip_unprivileged_port_start=1024 || true
  fi
  start_dockwell
  wait_for_dockwell
else
  echo "E2E_PANEL isn't set, so moving the panel's domain behind Caddy wasn't checked on real Docker. CI sets it"
fi

step "Ingress: a token can't take a domain another environment held"
dw environments create staging --yes || fail "environments create staging failed"
dw services create web --environment staging --app-image nginx:1.27-alpine --yes || fail "services create web in staging failed"
set +e
taken=$(dw routes create www --environment staging --domain "$domain" --service web --port 80 --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "taking a domain from another environment exited $status, want 4: it waits for a person: $taken"
grep -q 'the environment production held' <<<"$taken" || fail "the plan doesn't say whose domain it takes: $taken"
grep -q 'takes a domain another environment held, so it needs a person' <<<"$taken" || fail "the CLI doesn't say why it needs a person: $taken"
if docker inspect dockwell-ingress >/dev/null 2>&1; then fail "the ingress started for a plan that waits for a person"; fi
changes=$(dw changes list --json) || fail "changes list failed"
takeover=$(jq -r '.changes[0].id' <<<"$changes")
# The CLI stops short of asking, so this is the server's own refusal.
token_refused "$takeover"
dw changes discard "$takeover" >/dev/null || fail "changes discard failed"
dw services delete web --environment staging --yes || fail "services delete web in staging failed"
dw environments delete staging --yes || fail "environments delete staging failed"

dw services delete web --yes || fail "services delete web failed"
in_ingress=""

step "A file in the shapes real ones have runs, as a person approves it"
real_port=$((web_port + 1))
docker network create e2e-edge >/dev/null || fail "docker network create e2e-edge failed"
dw environments create real --yes >/dev/null || fail "environments create real failed"
dw environments create other --yes >/dev/null || fail "environments create other failed"
cat >"$data/real.yaml" <<'EOF'
services:
  shop:
    container_name: e2e-storefront
    image: nginx:1.27-alpine
    build: ./not-read
    deploy:
      resources:
        limits:
          cpus: ${CPUS}
          memory: ${MEMORY_LIMIT}
        reservations:
          cpus: "0.25"
          memory: 16M
    cap_add: [NET_ADMIN]
    cap_drop: [MKNOD]
    group_add: ["12345"]
    shm_size: 128m
    tmpfs: ["/run/e2e-short:size=8m"]
    labels:
      createdBy: Apps
    networks: [e2e-edge]
    ports:
      - ${HOST_IP}:${PANEL_APP_PORT_HTTP}:80
    restart: on-failure:5
    healthcheck:
      test: ["CMD-SHELL", "wget -q -O /dev/null http://127.0.0.1/ || exit 1"]
      interval: 2s
      timeout: 2s
      retries: 15
    volumes:
      - ./data:/data
      - {type: tmpfs, target: /run/e2e-long, tmpfs: {size: 4m}}
networks:
  e2e-edge:
    external: true
EOF
printf 'CPUS=0.50\nMEMORY_LIMIT=64M\nHOST_IP=\nPANEL_APP_PORT_HTTP=%s\n' "$real_port" >"$data/real.env"
import_real() { dw compose import --compose "$(cat "$data/real.yaml")" --env-file "$data/real.env" --directory "$real_dir" --project default --environment real "$@"; }

planned=$(import_real --plan-only --json) || fail "importing the file failed"
change=$(jq -r '.change.id // empty' <<<"$planned") || fail "compose import printed what isn't JSON: $planned"
[[ "$change" == chg_* ]] || fail "compose import planned no change: $planned"
jq -e '.change.plan.hostAccess == true' <<<"$planned" >/dev/null || fail "the file plans no host access: $planned"
for says in 'e2e-edge' 'NET_ADMIN' '12345' "$real_dir/data"; do
  [[ "$planned" == *"$says"* ]] || fail "the plan doesn't name $says: $planned"
done
for reason in container_name no_build cpu_reservation; do
  jq -e --arg r "$reason" 'any(.change.plan.notes[]; .reason == $r)' <<<"$planned" >/dev/null || fail "the plan doesn't say $reason: $planned"
done
token_refused "$change"
person_apply "$change"

shop=$(docker ps --filter label=com.docker.compose.service=shop --format '{{.Names}}') || fail "docker ps failed"
[[ "$shop" == default.real.shop-1 ]] || fail "Docker runs '$shop' for shop, want default.real.shop-1, not the file's container_name"
inspected=$(docker inspect "$shop") || fail "docker inspect $shop failed"
real_projects+=("$(jq -r '.[0].Config.Labels["com.docker.compose.project"]' <<<"$inspected")")
has() { jq -e "$1" <<<"$inspected" >/dev/null || fail "$shop isn't as asked: $1 is $(jq -c "$1" <<<"$inspected"), in $(jq -c '.[0].HostConfig' <<<"$inspected")"; }
has '.[0].HostConfig.NanoCpus == 500000000'
has '.[0].HostConfig.Memory == 67108864'
has '.[0].HostConfig.MemoryReservation == 16777216'
has '.[0].HostConfig.CapAdd | any(endswith("NET_ADMIN"))'
has '.[0].HostConfig.CapDrop | any(endswith("MKNOD"))'
has '.[0].HostConfig.GroupAdd | index("12345") != null'
has '.[0].HostConfig.ShmSize == 134217728'
has '.[0].HostConfig.Tmpfs["/run/e2e-short"] == "size=8388608"'
has '.[0].HostConfig.Tmpfs["/run/e2e-long"] == "size=4194304"'
has '.[0].HostConfig.RestartPolicy.Name == "on-failure" and .[0].HostConfig.RestartPolicy.MaximumRetryCount == 5'
has '.[0].NetworkSettings.Networks["e2e-edge"].Aliases | (index("e2e-storefront") != null and index("shop") != null)'
has '.[0].NetworkSettings.Networks | keys == ["e2e-edge"]'
has '.[0].HostConfig.Binds | length == 1'
bind=$(jq -r '.[0].HostConfig.Binds[0]' <<<"$inspected")
[[ "$bind" == "$real_dir/data:/data" ]] || fail "./data mounts as '$bind', want $real_dir/data:/data"
# What Docker was told is one thing, and what the container has another.
held=$(docker exec "$shop" df -kP /dev/shm /run/e2e-short /run/e2e-long) || fail "df in $shop failed"
for want in '/dev/shm 131072' '/run/e2e-short 8192' '/run/e2e-long 4096'; do
  got=$(awk -v at="${want% *}" '$NF == at { print $NF, $2 }' <<<"$held")
  [[ "$got" == "$want" ]] || fail "$shop has '$got', want '$want' KiB, in: $held"
done
page=$(curl -fsS --max-time 10 "http://127.0.0.1:$real_port/") || fail "shop doesn't serve on port $real_port, on the empty HOST_IP's every address"
[[ "$page" == *nginx* ]] || fail "shop serves something other than nginx on port $real_port: ${page:0:300}"
drift=$(dw drift list --json) || fail "drift list failed"
# Another environment has drift of its own by now, as an ingress step above prunes
# a network it left unused, so this asks about shop alone.
jq -e 'any((.drift // [])[]; .resource.name == "shop") | not' <<<"$drift" >/dev/null || fail "drift with shop running as asked: $drift"

step "An image for another architecture says so, where Docker reports only a container that exited"
here=$(docker version --format '{{.Server.Arch}}') || fail "docker version failed"
other_arch=arm64
[[ "$here" != arm64 ]] || other_arch=amd64
docker pull -q --platform "linux/$other_arch" alpine:3.20 >/dev/null || fail "can't pull an image for $other_arch"
docker tag alpine:3.20 e2e-wrong-arch:1 || fail "can't tag the image for $other_arch"
docker rmi alpine:3.20 >/dev/null || fail "can't remove the image's own tag"
# A server with emulation runs it, so nothing goes wrong, and this step proves nothing.
if probe=$(docker run --rm --platform "linux/$other_arch" e2e-wrong-arch:1 true 2>&1); then
  echo "docker-e2e: this machine runs $other_arch images, so there's no wrong architecture to read, and this step proves nothing"
else
  echo "docker-e2e: of an image for $other_arch on $here, Docker says: ${probe:0:300}"
  if said=$(dw services create wrongarch --environment other --app-image e2e-wrong-arch:1 --yes 2>&1); then
    fail "an image for $other_arch started on $here: $said"
  fi
  [[ "$said" == *"isn't built for this server's architecture"* ]] || fail "an image for $other_arch failed saying: $said"
  if [[ "$said" == *"Check its command"* ]]; then fail "an image for $other_arch sends you to its command: $said"; fi
  # The CLI ends as the rollback begins, and the image can't go while that runs.
  for _ in $(seq 300); do
    [[ -z "$(docker ps -aq --filter label=com.docker.compose.service=wrongarch)" ]] && break
    sleep 0.1
  done
  [[ -z "$(docker ps -aq --filter label=com.docker.compose.service=wrongarch)" ]] || fail "the rollback left what failed: $(docker ps -a)"
fi
docker image rm e2e-wrong-arch:1 >/dev/null || true

step "A limit changes in place, on a token, and the file's container stays"
before=$(jq -r '.[0].Id' <<<"$inspected")
started=$(jq -r '.[0].State.StartedAt' <<<"$inspected")
import_real --variables MEMORY_LIMIT=128M --yes >/dev/null || fail "changing the limit failed"
inspected=$(docker inspect "$shop") || fail "docker inspect $shop failed"
[[ "$(jq -r '.[0].Id' <<<"$inspected")" == "$before" ]] || fail "changing a limit recreated $shop"
[[ "$(jq -r '.[0].State.StartedAt' <<<"$inspected")" == "$started" ]] || fail "changing a limit restarted $shop"
has '.[0].HostConfig.Memory == 134217728'
has '.[0].HostConfig.MemorySwap == 268435456'

step "A limit lifted takes a new container, as Docker can't lift one in place"
limited_file() { printf 'services:\n  limited:\n    image: nginx:1.27-alpine\n%s' "$1"; }
limited_id() { docker ps --filter label=com.docker.compose.service=limited --format '{{.ID}}'; }
dw compose import --compose "$(limited_file $'    deploy:\n      resources:\n        limits:\n          memory: 64M\n')" --project default --environment other --yes >/dev/null ||
  fail "importing a limit failed"
before=$(limited_id) || fail "docker ps failed"
[[ -n "$before" ]] || fail "Docker runs nothing for limited"
real_projects+=("$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$before")")
[[ "$(docker inspect --format '{{.HostConfig.Memory}}' "$before")" == 67108864 ]] || fail "limited runs without its limit: $(docker inspect --format '{{json .HostConfig}}' "$before")"
dw compose import --compose "$(limited_file '')" --project default --environment other --yes >/dev/null || fail "lifting the limit failed"
after=$(limited_id) || fail "docker ps failed"
[[ -n "$after" && "$after" != "$before" ]] || fail "lifting the limit left the container '$after', which was '$before'"
[[ "$(docker inspect --format '{{.HostConfig.Memory}}' "$after")" == 0 ]] || fail "limited keeps a limit: $(docker inspect --format '{{json .HostConfig}}' "$after")"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e 'any((.drift // [])[]; .resource.name == "limited") | not' <<<"$drift" >/dev/null || fail "drift with the limit lifted: $drift"
dw services delete limited --project default --environment other --yes >/dev/null || fail "services delete limited failed"

step "A privileged service needs a person, runs privileged and isn't drift"
privileged_file=$(printf 'services:\n  privileged:\n    image: nginx:1.27-alpine\n    privileged: true\n')
planned=$(dw compose import --compose "$privileged_file" --project default --environment other --plan-only --json) || fail "importing a privileged service failed"
change=$(jq -r '.change.id // empty' <<<"$planned") || fail "compose import printed what isn't JSON: $planned"
[[ "$change" == chg_* ]] || fail "compose import planned no change: $planned"
jq -e '.change.plan.hostAccess == true' <<<"$planned" >/dev/null || fail "a privileged service plans no host access: $planned"
token_refused "$change"
person_apply_root "$change"
privileged=$(docker ps --filter label=com.docker.compose.service=privileged --format '{{.ID}}') || fail "docker ps failed"
[[ -n "$privileged" ]] || fail "Docker runs nothing for privileged"
real_projects+=("$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$privileged")")
[[ "$(docker inspect --format '{{.HostConfig.Privileged}}' "$privileged")" == true ]] || fail "the service runs without privileged"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e 'any((.drift // [])[]; .resource.name == "privileged") | not' <<<"$drift" >/dev/null || fail "drift with a privileged service running: $drift"
dw services delete privileged --project default --environment other --yes >/dev/null || fail "services delete privileged failed"

step "It exports as a file docker compose reads, naming the network as external"
dw compose export --environment real >"$data/real-export.yaml" || fail "compose export failed"
grep -q 'external: true' "$data/real-export.yaml" || fail "the export doesn't name its network as external:
$(cat "$data/real-export.yaml")"
docker compose -f "$data/real-export.yaml" config --quiet || fail "docker compose doesn't take the export:
$(cat "$data/real-export.yaml")"
listed=$(docker compose -f "$data/real-export.yaml" ps --format '{{.Service}}') || fail "docker compose -f <export> ps failed"
[[ "$listed" == shop ]] || fail "docker compose -f <export> ps lists '$listed', want shop"

step "A plan says which driver a network it didn't make uses"
# With no parent, Docker gives a macvlan network a dummy interface of its own.
docker network create --driver macvlan e2e-lan >/dev/null || fail "docker network create e2e-lan failed"
lan_file=$(printf 'services:\n  lan:\n    image: nginx:1.27-alpine\n    networks: [e2e-lan]\nnetworks:\n  e2e-lan:\n    external: true\n')
planned=$(dw compose import --compose "$lan_file" --project default --environment other --plan-only --json) || fail "importing a service on a macvlan network failed"
jq -e '[.change.plan.servers[].steps[].hostAccess[]? | select(.path == "e2e-lan")] | any(.leadsTo == "macvlan")' <<<"$planned" >/dev/null ||
  fail "the plan doesn't say e2e-lan is a macvlan network: $planned"
read_plan=$(dw changes get "$(jq -r .change.id <<<"$planned")") || fail "changes get failed"
grep -qF "the macvlan network e2e-lan, which reaches past the server's firewall" <<<"$read_plan" || fail "the plan doesn't say so in words: $read_plan"
docker network rm e2e-lan >/dev/null || fail "docker network rm e2e-lan failed"

step "Another project on the network reaches it by name, and can't take that name"
docker tag nginx:1.27-alpine e2e-local-only:1 || fail "docker tag failed"
other_file=$(printf 'services:\n  client:\n    image: e2e-local-only:1\n    build: .\n    networks: [e2e-edge]\nnetworks:\n  e2e-edge:\n    external: true\n')
planned=$(dw compose import --compose "$other_file" --project default --environment other --plan-only --json) || fail "importing a local-only image failed"
change=$(jq -r '.change.id // empty' <<<"$planned") || fail "compose import printed what isn't JSON: $planned"
token_refused "$change"
person_apply "$change"
client=$(docker ps --filter label=com.docker.compose.service=client --format '{{.Names}}') || fail "docker ps failed"
[[ -n "$client" ]] || fail "Docker runs nothing for client"
real_projects+=("$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$client")")
page=$(docker exec "$client" wget -q -O - -T 5 http://e2e-storefront/) || fail "client can't reach shop by its container_name on e2e-edge"
[[ "$page" == *nginx* ]] || fail "client reached something other than shop: ${page:0:300}"
page=$(docker exec "$shop" wget -q -O - -T 5 http://client/) || fail "shop can't reach client by its name on e2e-edge"
[[ "$page" == *nginx* ]] || fail "shop reached something other than client: ${page:0:300}"
clash=$(printf 'services:\n  clash:\n    image: nginx:1.27-alpine\n    container_name: e2e-storefront\n    networks: [e2e-edge]\nnetworks:\n  e2e-edge:\n    external: true\n')
if out=$(dw compose import --compose "$clash" --project default --environment other --plan-only 2>&1); then
  fail "a second container answering to e2e-storefront on e2e-edge was planned: ${out:0:300}"
fi
[[ "$out" == *"already does"* ]] || fail "a name another has failed some other way: ${out:0:300}"

step "A stopped container keeps its name on the network"
# Docker keeps a stopped container on its networks, with its aliases, so stopping one
# frees nothing, and starting it would have two answer to one name.
docker stop --time 1 "$shop" >/dev/null || fail "docker stop $shop failed"
if out=$(dw compose import --compose "$clash" --project default --environment other --plan-only 2>&1); then
  fail "a name a stopped container has was planned: ${out:0:300}"
fi
[[ "$out" == *"already does"* ]] || fail "a name a stopped container has failed some other way: ${out:0:300}"
docker start "$shop" >/dev/null || fail "docker start $shop failed"
for _ in $(seq 150); do
  [[ "$(docker inspect --format '{{.State.Health.Status}}' "$shop")" == healthy ]] && break
  sleep 0.2
done
[[ "$(docker inspect --format '{{.State.Health.Status}}' "$shop")" == healthy ]] ||
  fail "$shop didn't turn healthy after starting again: $(docker inspect --format '{{.State.Status}}, exit code {{.State.ExitCode}}, health {{json .State.Health}}' "$shop" 2>&1)
$(docker logs --tail 20 "$shop" 2>&1)"

step "What isn't on the server is refused as it plans, by name"
missing=$(printf 'services:\n  gone:\n    image: nginx:1.27-alpine\n    networks: [e2e-missing]\nnetworks:\n  e2e-missing:\n    external: true\n')
if out=$(dw compose import --compose "$missing" --project default --environment other --plan-only 2>&1); then
  fail "a network the server hasn't got was planned: ${out:0:300}"
fi
[[ "$out" == *"the network e2e-missing isn't on this server"* ]] || fail "a missing network failed some other way: ${out:0:300}"

in_ingress=1
step "Ingress: a route with no certificate reaches a service on a network the server has, and isn't drift"
shop_site=shop.e2e-dockwell.example.net
dw routes create shop-site --environment real --domain "$shop_site" --service shop --port 80 --tls off --yes >/dev/null || fail "a route to a service on a network the server has failed"
page=$(curl -fsS --max-time 10 --resolve "$shop_site:80:127.0.0.1" "http://$shop_site/") || fail "http://$shop_site/ doesn't answer through Caddy"
[[ "$page" == *nginx* ]] || fail "http://$shop_site/ answers something other than shop: ${page:0:300}"
networks=$(docker inspect --format '{{json .NetworkSettings.Networks}}' "$shop") || fail "docker inspect $shop failed"
jq -e 'has("e2e-edge") and has("dockwell_ingress")' <<<"$networks" >/dev/null || fail "$shop isn't on its network and the ingress's: $networks"
drift=$(dw drift list --json) || fail "drift list failed"
jq -e 'any((.drift // [])[]; .resource.name == "shop" or .resource.name == "shop-site") | not' <<<"$drift" >/dev/null || fail "drift with shop routed: $drift"
dw routes delete shop-site --environment real --yes >/dev/null || fail "routes delete shop-site failed"

step "Ingress: a service on the server's own network takes a route by its port, and not by its name"
on_host_port=$((web_port + 2))
on_host_site=onhost.e2e-dockwell.example.net
# nginx's image listens on port 80, which Caddy holds on this network.
on_host_file=$(cat <<EOF
services:
  onhost:
    image: nginx:1.27-alpine
    network_mode: host
    command:
      - sh
      - -c
      - |-
        sed -i -E '/\[::\]:80;/d; s/listen +80;/listen 127.0.0.1:$on_host_port;/' /etc/nginx/conf.d/default.conf && exec nginx -g 'daemon off;'
EOF
)
planned=$(dw compose import --compose "$on_host_file" --project default --environment real --plan-only --json) || fail "importing a service on the server's own network failed"
on_host_change=$(jq -r '.change.id // empty' <<<"$planned") || fail "compose import printed what isn't JSON: $planned"
[[ "$on_host_change" == chg_* ]] || fail "compose import planned no change: $planned"
person_apply_root "$on_host_change"
for _ in $(seq 100); do
  curl -fsS --max-time 1 -o /dev/null "http://127.0.0.1:$on_host_port/" 2>/dev/null && break
  sleep 0.1
done
curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:$on_host_port/" || fail "onhost doesn't answer on port $on_host_port: $(docker ps -a --filter label=com.docker.compose.service=onhost)"
if out=$(dw routes create onhost --environment real --domain "$on_host_site" --service onhost --port 80 --tls off --yes 2>&1); then
  fail "a route by name to a service on the server's own network was planned: ${out:0:300}"
fi
[[ "$out" == *"runs on the server's own network"* && "$out" == *host_port* ]] || fail "a route by name to a service on the server's own network failed some other way: ${out:0:300}"
set +e
planned=$(dw routes create onhost --environment real --domain "$on_host_site" --host-port "$on_host_port" --tls off --yes 2>&1)
status=$?
set -e
[[ "$status" == 4 ]] || fail "a route to its port exited $status, want 4: it waits for a person: $planned"
[[ "$planned" == *"127.0.0.1:$on_host_port"* ]] || fail "the plan doesn't name the port: $planned"
changes=$(dw changes list --json) || fail "changes list failed"
reach=$(jq -r '.changes[0].id' <<<"$changes")
[[ "$reach" == chg_* ]] || fail "changes list has no change: $changes"
token_refused "$reach"
person_apply "$reach"
page=$(curl -fsS --max-time 10 --resolve "$on_host_site:80:127.0.0.1" "http://$on_host_site/") || fail "http://$on_host_site/ doesn't answer through Caddy"
[[ "$page" == *nginx* ]] || fail "http://$on_host_site/ answers something other than onhost: ${page:0:300}"
dw routes delete onhost --environment real --yes >/dev/null || fail "routes delete onhost failed"
dw services delete onhost --environment real --yes >/dev/null || fail "services delete onhost failed"

step "Ingress: a service with a route can't go onto the server's own network, and goes on serving"
moves_site=moves.e2e-dockwell.example.net
dw services create moves --environment real --app-image nginx:1.27-alpine --yes >/dev/null || fail "services create moves failed"
dw routes create moves --environment real --domain "$moves_site" --service moves --port 80 --tls off --yes >/dev/null || fail "routes create moves failed"
if out=$(dw services update moves --environment real --app-image nginx:1.27-alpine --app-host-network --plan-only 2>&1); then
  fail "taking a service with a route onto the server's own network was planned: ${out:0:300}"
fi
[[ "$out" == *"has a route"* && "$out" == *host_port* ]] || fail "taking a service with a route onto the server's own network failed some other way: ${out:0:300}"
page=$(curl -fsS --max-time 10 --resolve "$moves_site:80:127.0.0.1" "http://$moves_site/") || fail "http://$moves_site/ stopped answering"
[[ "$page" == *nginx* ]] || fail "http://$moves_site/ answers something else: ${page:0:300}"
dw routes delete moves --environment real --yes >/dev/null || fail "routes delete moves failed"
dw services delete moves --environment real --yes >/dev/null || fail "services delete moves failed"
in_ingress=""

step "Going leaves the network as it was"
dw changes undo "$change" --yes >/dev/null || fail "changes undo $change failed"
if docker inspect "$client" >/dev/null 2>&1; then fail "the undo left $client"; fi
dw services delete shop --environment real --yes >/dev/null || fail "services delete shop failed"
if docker inspect "$shop" >/dev/null 2>&1; then fail "deleting shop left $shop"; fi
docker network inspect e2e-edge >/dev/null || fail "the undo or the deletion took the network e2e-edge, which Dockwell didn't make"

step "A container made and not yet started holds its name on a network"
# As another change's does between making it and starting it, for as long as a
# stop-first deploy takes to stop the old one. Docker has a network's ID for a
# container only once it first starts, so only asking by the network's name finds it.
docker create --name e2e-not-started --network e2e-edge --network-alias e2e-unstarted nginx:1.27-alpine >/dev/null || fail "docker create failed"
edge_id=$(docker network inspect --format '{{.Id}}' e2e-edge) || fail "docker network inspect e2e-edge failed"
[[ -n "$(docker ps -aq --filter network=e2e-edge --filter name=e2e-not-started)" ]] || fail "Docker doesn't list a container not yet started on its network, by name"
[[ -z "$(docker ps -aq --filter "network=$edge_id" --filter name=e2e-not-started)" ]] || fail "Docker lists a container not yet started by the network's ID, which the check needn't avoid"
unstarted=$(printf 'services:\n  unstarted:\n    image: nginx:1.27-alpine\n    container_name: e2e-unstarted\n    networks: [e2e-edge]\nnetworks:\n  e2e-edge:\n    external: true\n')
if out=$(dw compose import --compose "$unstarted" --project default --environment other --plan-only 2>&1); then
  fail "a name a container not yet started holds was planned: ${out:0:300}"
fi
[[ "$out" == *"already does"* ]] || fail "a name a container not yet started holds failed some other way: ${out:0:300}"
docker rm e2e-not-started >/dev/null || fail "docker rm e2e-not-started failed"

# Last of what names a project: its projects stay, as nothing deletes one, and
# with more than one, everything that names none is refused for not saying which.
step "Two Compose files with a service of one name run side by side"
# With nothing to say where it goes, a file is refused now the default project
# has what it would change.
dw services create keeper --environment production --app-image nginx:1.27-alpine --yes || fail "services create keeper failed"
if out=$(dw compose import --compose "services: {keeper: {image: nginx:1.28-alpine}}" --environment production --yes 2>&1); then
  fail "a file with no name was imported into a project with apps: $out"
fi
grep -q "names no project" <<<"$out" || fail "a file with no name was refused saying: $out"
named_file() { printf 'name: %s\nservices:\n  db:\n    image: %s\n' "$1" "$2"; }
dw compose import --compose "$(named_file blog nginx:1.27-alpine)" --yes || fail "importing blog failed"
dw compose import --compose "$(named_file wiki nginx:1.28-alpine)" --yes || fail "importing wiki failed"
dbs=$(docker ps --filter label=com.docker.compose.service=db --format '{{.Names}} {{.Image}} {{.Networks}}') || fail "docker ps failed"
[[ "$(wc -l <<<"$dbs")" == 2 ]] || fail "Docker runs these dbs, want two: $dbs"
project_of() { docker ps --filter label=com.docker.compose.service=db --filter "ancestor=$1" --format '{{.Label "com.docker.compose.project"}}'; }
blog_project=$(project_of nginx:1.27-alpine) || fail "docker ps failed"
wiki_project=$(project_of nginx:1.28-alpine) || fail "docker ps failed"
extra_projects="$blog_project $wiki_project"
[[ -n "$blog_project" && -n "$wiki_project" && "$blog_project" != "$wiki_project" ]] ||
  fail "the two files' dbs are in projects '$blog_project' and '$wiki_project', want one each"
members=$(docker network inspect --format '{{range .Containers}}{{.Name}} {{end}}' "${blog_project}_default") ||
  fail "blog has no network of its own"
[[ "$members" != *"$wiki_project"* ]] || fail "wiki's db is on blog's network: $members"
docker network inspect "${wiki_project}_default" >/dev/null || fail "wiki has no network of its own"
listed=$(docker compose -p "$blog_project" ps --format '{{.Service}}') || fail "docker compose -p $blog_project ps failed"
[[ "$listed" == db ]] || fail "docker compose lists '$listed' in blog's project, want db"

step "Ingress: each project's route reaches its own service, with one name between them, through one Caddy"
blog_site=blog.e2e-dockwell.example.net
wiki_site=wiki.e2e-dockwell.example.net
# In the container, and not its spec: all that's needed is to tell them apart.
blog_db=$(service_container "$blog_project" db)
wiki_db=$(service_container "$wiki_project" db)
docker exec "$blog_db" sh -c 'echo blog >/usr/share/nginx/html/index.html' || fail "can't write in blog's db, $blog_db"
docker exec "$wiki_db" sh -c 'echo wiki >/usr/share/nginx/html/index.html' || fail "can't write in wiki's db, $wiki_db"
dw routes create site --project blog --domain "$blog_site" --service db --port 80 --tls off --yes >/dev/null || fail "routes create for blog failed"
dw routes create site --project wiki --domain "$wiki_site" --service db --port 80 --tls off --yes >/dev/null || fail "routes create for wiki failed"
site_serves() {
  local got=""
  for _ in $(seq 100); do
    got=$(curl -sS --max-time 2 --resolve "$1:80:127.0.0.1" "http://$1/" 2>/dev/null) || got=""
    [[ "$got" == "$2" ]] && return 0
    sleep 0.1
  done
  fail "http://$1/ serves '$got', want '$2'"
}
site_serves "$blog_site" blog
site_serves "$wiki_site" wiki
# One going leaves the other's.
dw routes delete site --project blog --yes >/dev/null || fail "routes delete for blog failed"
site_serves "$wiki_site" wiki
dw routes delete site --project wiki --yes >/dev/null || fail "routes delete for wiki failed"

# Naming one again updates it, and doesn't touch the other.
wiki_state() { docker inspect --format '{{.Id}} {{.Config.Image}} {{.State.StartedAt}}' "$wiki_db"; }
wiki_before=$(wiki_state) || fail "can't read wiki's db, $wiki_db"
dw compose import --compose "$(named_file blog nginx:1.28-alpine)" --yes || fail "importing blog again failed"
again=$(docker ps --filter "label=com.docker.compose.project=$blog_project" --format '{{.Image}}') || fail "docker ps failed"
[[ "$again" == nginx:1.28-alpine ]] || fail "after importing blog again, its db runs '$again'"
wiki_after=$(wiki_state) || fail "after importing blog again, wiki's db is gone"
[[ "$wiki_after" == "$wiki_before" ]] || fail "importing blog again touched wiki's db: $wiki_before, then $wiki_after"
dw services delete db --project blog --yes || fail "deleting blog's db failed"
dw services delete db --project wiki --yes || fail "deleting wiki's db failed"
dw services delete keeper --project default --environment production --yes || fail "deleting keeper failed"

# catalog_install installs an app through the CLI, and says which Compose project it
# made in catalog_project.
catalog_install() {
  local planned change environment
  planned=$(dw catalog install "$@" --plan-only --json) || fail "planning to install $1 failed"
  change=$(jq -r '.change.id // empty' <<<"$planned") || fail "planning to install $1 printed what isn't JSON: $planned"
  [[ "$change" == chg_* ]] || fail "installing $1 planned no change: $planned"
  environment=$(jq -r '[.change.plan.resources[] | select(.resource.kind == "service")][0].resource.scope' <<<"$planned") ||
    fail "the plan to install $1 names no service: $planned"
  catalog_project=${environment,,}
  real_projects+=("$catalog_project")
  dw changes apply "$change" --yes >/dev/null || fail "installing $1 failed"
}
# catalog_container names what a service of the last install runs in, once it's up.
catalog_container() {
  local out
  out=$(docker ps --filter "label=com.docker.compose.project=$catalog_project" --filter "label=com.docker.compose.service=$1" --format '{{.Names}}') ||
    fail "docker ps failed"
  [[ -n "$out" && "$out" != *$'\n'* ]] || fail "Docker runs '$out' for $1, want one container"
  echo "$out"
}
catalog_healthy() {
  local health
  health=$(docker inspect --format '{{.State.Health.Status}}' "$1") || fail "docker inspect $1 failed"
  [[ "$health" == healthy ]] || fail "$1 reads $health once its install has applied, want healthy: $(docker logs --tail 20 "$1" 2>&1)"
}
catalog_pinned() {
  local image
  image=$(docker inspect --format '{{.Config.Image}}' "$1") || fail "docker inspect $1 failed"
  [[ "$image" == *@sha256:* ]] || fail "$1 runs $image, not pinned by digest"
}
# catalog_value reads what an install's input was given, and catalog_env what a
# container has, which is how a person's passkey would see a generated secret.
catalog_value() {
  local installs value
  installs=$(dw catalog installs --json) || fail "catalog installs failed"
  value=$(jq -r --arg p "$1" --arg k "$2" '.installs[] | select(.project == $p) | .pin.values[$k] // empty' <<<"$installs") ||
    fail "catalog installs printed what isn't JSON: $installs"
  [[ -n "$value" ]] || fail "the install $1 keeps no $2: $installs"
  echo "$value"
}
catalog_env() {
  local all
  all=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$1") || fail "docker inspect $1 failed"
  sed -n "s/^$2=//p" <<<"$all"
}
# form_token reads the token a login form carries, by its first mention. Adminer and
# phpMyAdmin each take a sign-in only with it, and without it show the form again and
# say nothing, as a form on another site can't have it. A browser sends it unasked.
form_token() {
  sed -n "/name=[\"']token[\"']/{s/.*name=[\"']token[\"'] value=[\"']\([^\"']*\)[\"'].*/\1/p;q;}" <<<"$1"
}
# sign_in_says prints why a sign-in failed: the page's title and what it says of it,
# which a page's first lines stop short of. $2 is the password, which a log never holds.
sign_in_says() {
  local page=$1 title="" words=""
  [[ "$page" =~ \<title\>([^\<]*) ]] && title=${BASH_REMATCH[1]}
  [[ "$page" =~ \<div\ class=\'error\'\>([^\<]*) ]] && words=${BASH_REMATCH[1]}
  [[ "$page" =~ (Cannot\ log\ in[^\<]*) ]] && words=${BASH_REMATCH[1]}
  [[ "$page" =~ (real_connect[^\<]*) ]] && words+=" ${BASH_REMATCH[1]}"
  words="the page $title says: ${words:-nothing}"
  echo "${words//"$2"/<the password>}"
}
# catalog_generated reads a generated secret, and says nothing but that shows it.
catalog_generated() {
  local value
  value=$(catalog_env "$1" "$2")
  [[ ${#value} -ge 32 ]] || fail "$1 got ${#value} characters for $2, want a generated value"
  echo "$value"
}

# volume_id finds a volume by its name and its environment's Compose project.
volume_id() {
  local listed
  listed=$(dw resources list --kind volume --json) || fail "listing volumes failed"
  jq -r --arg name "$1" --arg project "$2" \
    '.resources[] | select(.resource.name == $name and (.resource.scope | ascii_downcase) == $project) | .id' <<<"$listed"
}
# backup_field reads a field of a backup.
backup_field() { dw backups get "$1" --json | jq -r ".backup.$2"; }
# backup_take backs up a volume, waits for it, says its ID, and fails unless it's
# whole. It leaves nothing of its helper behind, and Docker no copy in a log.
backup_take() {
  local made id state="" left
  made=$(dw backups create --volume-id "$1" "${@:2}" --yes --json) || fail "backing up $1 failed: $made"
  id=$(jq -r .backup.id <<<"$made") || fail "backups create printed what isn't JSON: $made"
  for _ in $(seq 240); do
    state=$(backup_field "$id" state) || state=""
    [[ "$state" != BACKUP_STATE_RUNNING ]] && break
    sleep 0.5
  done
  [[ "$state" == BACKUP_STATE_DONE ]] || fail "backup $id is $state: $(dw backups get "$id" --json)"
  left=$(docker ps --all --quiet --filter label=dev.dockwell.volume-tool; docker images --quiet dockwell-volume-tool) || fail "docker ps failed"
  [[ -z "$left" ]] || fail "a backup left its helper behind: $left"
  echo "$id"
}
# backup_copy waits for what became of a backup's copy, which goes once it's whole, and
# says it.
backup_copy() {
  local copy=""
  for _ in $(seq 240); do
    copy=$(backup_field "$1" copy) || copy=""
    [[ "$copy" != BACKUP_COPY_PENDING ]] && break
    sleep 0.5
  done
  echo "$copy"
}
# backup_plan plans restoring a backup, and says its change. A token can plan it and
# not apply it, and it says it destroys data and what stops.
backup_plan() {
  local planned change
  planned=$(dw backups restore "$1" --plan-only --json) || fail "planning to restore $1 failed: $planned"
  jq -e --argjson stops "$2" '.change.plan.destructive == true and (.change.plan.servers[0].steps[0] | .destroysData == true and .restore.stops == $stops)' <<<"$planned" >/dev/null ||
    fail "the plan doesn't say what it replaces and that $2 stop: $planned"
  change=$(jq -r .change.id <<<"$planned")
  token_refused "$change"
  echo "$change"
}
# backup_healthy waits for what a restore started to be healthy again.
backup_healthy() {
  local health=""
  for _ in $(seq 180); do
    health=$(docker inspect --format '{{.State.Health.Status}}' "$1") || fail "docker inspect $1 failed"
    [[ "$health" == healthy ]] && return 0
    sleep 0.5
  done
  fail "$1 reads $health 90 s after a restore, want healthy: $(docker logs --tail 20 "$1" 2>&1)"
}
# backup_undo undoes a restore, which a person has to apply too.
backup_undo() {
  local planned change
  planned=$(dw changes undo "$1" --plan-only --json) || fail "planning to undo $1 failed: $planned"
  change=$(jq -r .change.id <<<"$planned")
  person_apply "$change"
}

step "Backups: a volume comes back as it was, owners, modes and links, through a plan"
backup_file=$(printf 'name: e2e-backups\nservices:\n  files:\n    image: nginx:1.27-alpine\n    volumes: [data:/srv/data]\nvolumes:\n  data: {}\n')
dw compose import --compose "$backup_file" --yes || fail "importing e2e-backups failed"
backup_files=$(docker ps --filter label=com.docker.compose.service=files --format '{{.Names}}') || fail "docker ps failed"
[[ -n "$backup_files" ]] || fail "e2e-backups runs nothing"
backup_project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$backup_files") || fail "docker inspect failed"
extra_projects+=" $backup_project"
backup_volume=$(volume_id data "$backup_project")
[[ -n "$backup_volume" ]] || fail "there's no volume data in $backup_project: $(dw resources list --kind volume --json)"
docker exec "$backup_files" sh -c '
  cd /srv/data && mkdir -p owned/deep shared empty &&
  echo the original >owned/file && echo private >owned/deep/key &&
  chown 1234:5678 owned/file && chmod 640 owned/file &&
  chown -R 999:998 owned/deep && chmod 600 owned/deep/key && chmod 700 owned/deep &&
  chmod 2750 owned && chmod 1777 shared && chown 33:33 . && chmod 750 . &&
  ln -s owned/file link && ln -s /etc/passwd outward && ln owned/file hard && mkfifo pipe && chmod 620 pipe &&
  touch -d "2020-02-03 04:05:06" owned/file' || fail "can't fill the volume"
# What a volume is, beyond what's in its files: each path's owner, mode, type, links and time.
backup_tree() { docker exec "$backup_files" sh -c 'cd /srv/data && find . -exec stat -c "%n %u:%g %a %F %h %N" {} + | sort; find . -type f -exec stat -c "%n %Y" {} + | sort'; }
tree_before=$(backup_tree) || fail "can't read the volume"
[[ "$tree_before" == *"./owned/file 1234:5678 640"* && "$tree_before" == *"./owned 0:0 2750"* && "$tree_before" == *". 33:33 750"* ]] ||
  fail "the volume isn't as this step made it: $tree_before"
said=$(dw backups preview --volume-id "$backup_volume" --json) || fail "previewing a backup failed: $said"
jq -e '.method == "BACKUP_METHOD_COPY_RUNNING" and .running == ["files"] and (.estimateBytes | tonumber > 0) and (.freeBytes | tonumber > 0)' <<<"$said" >/dev/null ||
  fail "the preview says: $said"
started=$(docker inspect --format '{{.State.StartedAt}}' "$backup_files") || fail "docker inspect failed"
backup=$(backup_take "$backup_volume" --stop)
[[ "$(backup_field "$backup" method)" == BACKUP_METHOD_COPY ]] || fail "a copy with the app stopped says $(backup_field "$backup" method)"
[[ "$(docker inspect --format '{{.State.Running}}' "$backup_files")" == true ]] || fail "what a backup stopped didn't start again"
[[ "$(docker inspect --format '{{.State.StartedAt}}' "$backup_files")" != "$started" ]] || fail "the backup said it stopped the app, and it didn't"
backup_file_on_disk="$data/agent/backups/$(jq -r .backup.environmentId <<<"$(dw backups get "$backup" --json)")/data/$backup.tar.gz"
# With nothing of Dockwell's: plain tar reads it, with the owners as numbers.
listing=$(priv tar --numeric-owner -tvzf "$backup_file_on_disk") || fail "plain tar can't read $backup_file_on_disk"
[[ "$listing" == *"1234/5678"* ]] || fail "the archive has no numeric owners: $listing"
mode=$(priv stat -c %a "$backup_file_on_disk") || fail "stat failed"
[[ "$mode" == 600 ]] || fail "the backup is readable by more than its owner: $mode"

docker exec "$backup_files" sh -c 'cd /srv/data && rm -rf owned link && echo made since >new && chmod 777 .' || fail "can't change the volume"
tree_changed=$(backup_tree) || fail "can't read the volume"
change=$(backup_plan "$backup" '["files"]')
person_apply "$change"
tree_after=$(backup_tree) || fail "can't read the volume"
[[ "$tree_after" == "$tree_before" ]] || fail "the volume didn't come back as it was:
$(diff <(printf '%s\n' "$tree_before") <(printf '%s\n' "$tree_after"))"
[[ "$(docker inspect --format '{{.State.Running}}' "$backup_files")" == true ]] || fail "the app didn't start again after a restore"
[[ "$(docker exec "$backup_files" cat /srv/data/owned/file)" == "the original" ]] || fail "the file's bytes didn't come back"
backup_undo "$change"
tree_undone=$(backup_tree) || fail "can't read the volume"
[[ "$tree_undone" == "$tree_changed" ]] || fail "undoing the restore didn't put back what it replaced:
$(diff <(printf '%s\n' "$tree_changed") <(printf '%s\n' "$tree_undone"))"

step "Backups: one goes sealed to S3-compatible storage, the server loses it, and it comes back"
s3_port=${E2E_S3_PORT:-19000}
s3_image=versity/versitygw:v1.9.0
s3_key=e2e-access-key
s3_secret=e2e-secret-that-signs-in
passphrase=e2e-a-passphrase-of-its-own
docker run --detach --name e2e-s3 --publish "127.0.0.1:$s3_port:7070" -e ROOT_ACCESS_KEY="$s3_key" -e ROOT_SECRET_KEY="$s3_secret" \
  "$s3_image" --port :7070 posix /tmp/vgw >/dev/null || fail "the S3 server didn't start"
for _ in $(seq 100); do
  curl -sS --max-time 2 -o /dev/null "http://127.0.0.1:$s3_port/" 2>/dev/null && break
  sleep 0.2
done
curl -fsS --max-time 10 --aws-sigv4 aws:amz:us-east-1:s3 --user "$s3_key:$s3_secret" -X PUT "http://127.0.0.1:$s3_port/e2e-backups" ||
  fail "can't make a bucket on the S3 server"
# From a file, as a command line stays in the process list.
jq -cn --arg endpoint "http://127.0.0.1:$s3_port" --arg key "$s3_key" --arg secret "$s3_secret" --arg phrase "$passphrase" \
  '{resource: {kind: "backup.destination", name: "e2e-s3"}, spec: {backupDestination: {endpoint: $endpoint, bucket: "e2e-backups", region: "us-east-1", accessKey: $key, secretKey: $secret, passphrase: $phrase}}}' \
  >"$data/destination.json" || fail "can't write the destination's edit"
planned=$(dw changes propose --summary "Send backups to an S3 server" --edits "@$data/destination.json" --plan-only --json) || fail "planning a destination failed: $planned"
[[ "$planned" != *"$s3_secret"* && "$planned" != *"$passphrase"* ]] || fail "the plan holds what signs in: $planned"
dw changes apply "$(jq -r .change.id <<<"$planned")" --yes >/dev/null || fail "applying a destination failed"
# Its own files: the step before ends by undoing a restore, which took these away.
docker exec "$backup_files" sh -c 'mkdir -p /srv/data/owned && echo the original >/srv/data/owned/file' || fail "can't write to the volume"
tree_before=$(backup_tree) || fail "can't read the volume"
backup=$(backup_take "$backup_volume" --stop --destination e2e-s3)
[[ "$(backup_copy "$backup")" == BACKUP_COPY_STORED ]] || fail "the copy says: $(dw backups get "$backup" --json)"
object_key=$(curl -fsS --max-time 10 --aws-sigv4 aws:amz:us-east-1:s3 --user "$s3_key:$s3_secret" "http://127.0.0.1:$s3_port/e2e-backups?list-type=2") ||
  fail "can't list the bucket"
[[ "$object_key" == *"$backup.tar.gz.sealed"* ]] || fail "the bucket holds: $object_key"
object=$(mktemp)
curl -fsS --max-time 30 --aws-sigv4 aws:amz:us-east-1:s3 --user "$s3_key:$s3_secret" -o "$object" \
  "http://127.0.0.1:$s3_port/e2e-backups/$(jq -r .backup.environmentId <<<"$(dw backups get "$backup" --json)")/data/$backup.tar.gz.sealed" || fail "can't read the object"
if tar -tzf "$object" >/dev/null 2>&1; then fail "what's in the bucket opens with no passphrase"; fi
beside=$(mktemp)
curl -fsS --max-time 30 --aws-sigv4 aws:amz:us-east-1:s3 --user "$s3_key:$s3_secret" -o "$beside" \
  "http://127.0.0.1:$s3_port/e2e-backups/$(jq -r .backup.environmentId <<<"$(dw backups get "$backup" --json)")/data/$backup.tar.gz.sealed.json" || fail "can't read what's beside the object"
if grep -aqe '"volume"' -e '"sha256"' "$beside"; then fail "what's beside the object can be read with no passphrase"; fi
rm -f "$beside"
# With nothing of Dockwell's but the command: the passphrase opens it, and plain tar reads it.
opened=$(mktemp)
rm -f "$opened"
DOCKWELL_BACKUP_PASSPHRASE=$passphrase "$bin" open-backup "$object" --output "$opened" || fail "dockwell open-backup can't open what's in the bucket"
listing=$(tar --numeric-owner -tvzf "$opened") || fail "what dockwell open-backup wrote isn't an archive"
[[ "$listing" == *"1234/5678"* ]] || fail "what came out has no numeric owners: $listing"
if DOCKWELL_BACKUP_PASSPHRASE=not-the-passphrase-at-all "$bin" open-backup "$object" --output "$opened.wrong" 2>/dev/null; then fail "the wrong passphrase opened a backup"; fi
rm -f "$object" "$opened"
priv rm -f "$data/agent/backups/$(jq -r .backup.environmentId <<<"$(dw backups get "$backup" --json)")/data/$backup.tar.gz" || fail "can't lose the server's copy"
docker exec "$backup_files" sh -c 'rm -rf /srv/data/owned' || fail "can't change the volume"
if said=$(dw backups restore "$backup" --plan-only --json 2>&1); then fail "a backup the server lost planned a restore: $said"; fi
dw backups fetch "$backup" --yes --json >/dev/null || fail "fetching the backup failed"
for _ in $(seq 240); do
  [[ -z "$(priv ls "$data/agent/backups/$(dw backups get "$backup" --json | jq -r .backup.environmentId)/data/$backup.tar.gz" 2>/dev/null)" ]] || break
  sleep 0.5
done
change=$(backup_plan "$backup" '["files"]')
person_apply "$change"
tree_after=$(backup_tree) || fail "can't read the volume"
[[ "$tree_after" == "$tree_before" ]] || fail "what came back from the S3 server isn't what went:
$(diff <(printf '%s\n' "$tree_before") <(printf '%s\n' "$tree_after"))"
said=$(grep -c -e "$s3_secret" -e "$passphrase" "$log" 2>/dev/null || true)
[[ "$said" == 0 ]] || fail "the log holds the destination's secret or its passphrase"
docker rm --force e2e-s3 >/dev/null || true

step "Catalog: PostgreSQL installs by digest with a password no one typed, and Adminer signs in with it"
catalog_install postgres
postgres_db=$(catalog_container db)
adminer=$(catalog_container adminer)
catalog_pinned "$postgres_db"
catalog_pinned "$adminer"
catalog_healthy "$postgres_db"
catalog_healthy "$adminer"
password=$(catalog_generated "$postgres_db" POSTGRES_PASSWORD)
seen=$(dw changes list --json; dw catalog installs --json; dw compose export --project postgres) || fail "reading the install back failed"
[[ "$seen" != *"$password"* ]] || fail "what anyone can read holds the generated password"
admin_port=$(catalog_value postgres ADMIN_PORT)
# As the entry's first steps say: PostgreSQL, the server it offers, and Database left empty.
adminer_sign_in() {
  local jar form token
  jar=$(mktemp)
  form=$(curl -fsS --max-time 10 -c "$jar" -b "$jar" "http://127.0.0.1:$admin_port/") || fail "Adminer doesn't answer on port $admin_port"
  [[ "$form" == *"name='auth[server]' value='db'"* ]] || fail "Adminer doesn't offer the server db: ${form:0:400}"
  token=$(form_token "$form")
  [[ -n "$token" ]] || fail "Adminer's login form has no token: ${form:0:400}"
  curl -sS -L --max-time 30 -c "$jar" -b "$jar" --data-urlencode "token=$token" --data-urlencode 'auth[driver]=pgsql' --data-urlencode 'auth[server]=db' \
    --data-urlencode 'auth[username]=postgres' --data-urlencode "auth[password]=$1" --data-urlencode 'auth[db]=' "http://127.0.0.1:$admin_port/" ||
    fail "signing in to Adminer got no answer"
  rm -f "$jar"
}
page=$(adminer_sign_in "$password")
[[ "$page" == *"id='logout'"* ]] || fail "Adminer can't sign in to the database with its password: $(sign_in_says "$page" "$password")"
page=$(adminer_sign_in not-the-password)
[[ "$page" != *"id='logout'"* ]] || fail "Adminer signed in with a wrong password"

step "Backups: PostgreSQL dumps with nothing stopped, and a restore brings back what a migration took"
pg() { docker exec "$postgres_db" psql -X -q -U postgres -d "$1" -tAc "$2"; }
pg postgres 'CREATE DATABASE shop' || fail "can't make a database"
pg postgres "CREATE ROLE shopper LOGIN PASSWORD 'shopper-password'" || fail "can't make a role"
# shellcheck disable=SC2016 # $$ quotes text for PostgreSQL.
pg shop 'CREATE TABLE orders (id int PRIMARY KEY, item text); INSERT INTO orders SELECT n, $$item $$ || n FROM generate_series(1, 5000) n; ALTER TABLE orders OWNER TO shopper' ||
  fail "can't fill the database"
postgres_volume=$(volume_id data "$catalog_project")
[[ -n "$postgres_volume" ]] || fail "there's no volume data in $catalog_project"
said=$(dw backups preview --volume-id "$postgres_volume" --json) || fail "previewing a backup failed: $said"
jq -e '.method == "BACKUP_METHOD_DUMP" and .database == "PostgreSQL"' <<<"$said" >/dev/null || fail "the preview says: $said"
started=$(docker inspect --format '{{.State.StartedAt}}' "$postgres_db") || fail "docker inspect failed"
backup=$(backup_take "$postgres_volume")
[[ "$(backup_field "$backup" method)" == BACKUP_METHOD_DUMP ]] || fail "PostgreSQL wasn't dumped: $(dw backups get "$backup" --json)"
[[ "$(docker inspect --format '{{.State.StartedAt}}' "$postgres_db")" == "$started" ]] || fail "a dump restarted the database"
pg shop 'DROP TABLE orders' || fail "can't drop the table"
pg postgres 'CREATE DATABASE made_since' || fail "can't make a database"
change=$(backup_plan "$backup" '["adminer","db"]')
person_apply "$change"
backup_healthy "$postgres_db"
backup_healthy "$adminer"
[[ "$(pg shop 'SELECT count(*) FROM orders')" == 5000 ]] || fail "the orders didn't come back: $(pg shop 'SELECT count(*) FROM orders' 2>&1)"
[[ "$(pg shop 'SELECT item FROM orders WHERE id = 42')" == "item 42" ]] || fail "the orders came back wrong"
[[ "$(pg shop "SELECT tableowner FROM pg_tables WHERE tablename = 'orders'")" == shopper ]] || fail "the table's owner didn't come back"
[[ "$(pg postgres "SELECT count(*) FROM pg_roles WHERE rolname = 'shopper'")" == 1 ]] || fail "the role didn't come back"
[[ "$(pg postgres "SELECT count(*) FROM pg_database WHERE datname = 'made_since'")" == 0 ]] || fail "what was made since the backup is still there"
page=$(adminer_sign_in "$password")
[[ "$page" == *"id='logout'"* ]] || fail "Adminer can't sign in after a restore: $(sign_in_says "$page" "$password")"
backup_undo "$change"
backup_healthy "$postgres_db"
[[ "$(pg postgres "SELECT count(*) FROM pg_database WHERE datname = 'made_since'")" == 1 ]] || fail "undoing the restore didn't bring back what it replaced"
[[ -z "$(pg shop "SELECT tablename FROM pg_tables WHERE tablename = 'orders'")" ]] || fail "undoing the restore left the orders"

step "Catalog: MySQL runs with a password of its own, and phpMyAdmin signs in with it"
catalog_install mysql
mysql_db=$(catalog_container db)
phpmyadmin=$(catalog_container phpmyadmin)
catalog_pinned "$mysql_db"
catalog_pinned "$phpmyadmin"
catalog_healthy "$mysql_db"
catalog_healthy "$phpmyadmin"
password=$(catalog_generated "$mysql_db" MYSQL_ROOT_PASSWORD)
answer=$(docker exec "$mysql_db" sh -c 'MYSQL_PWD=$MYSQL_ROOT_PASSWORD mysql -uroot -h127.0.0.1 -N -e "select 41 + 1"') ||
  fail "MySQL doesn't take the password it was given"
[[ "$answer" == 42 ]] || fail "MySQL answered '$answer'"
admin_port=$(catalog_value mysql ADMIN_PORT)
phpmyadmin_sign_in() {
  local jar form token
  jar=$(mktemp)
  form=$(curl -fsS --max-time 15 -c "$jar" -b "$jar" "http://127.0.0.1:$admin_port/") || fail "phpMyAdmin doesn't answer on port $admin_port"
  [[ "$form" == *pma_username* ]] || fail "port $admin_port answers something else: ${form:0:400}"
  token=$(form_token "$form")
  [[ -n "$token" ]] || fail "phpMyAdmin's login form has no token: ${form:0:400}"
  curl -sS -L --max-time 30 -c "$jar" -b "$jar" --data-urlencode "token=$token" --data-urlencode 'pma_username=root' --data-urlencode "pma_password=$1" \
    --data-urlencode 'server=1' --data-urlencode 'route=/' "http://127.0.0.1:$admin_port/index.php?route=/" || fail "signing in to phpMyAdmin got no answer"
  rm -f "$jar"
}
page=$(phpmyadmin_sign_in "$password")
[[ "$page" == *route=/logout* ]] || fail "phpMyAdmin can't sign in to MySQL with its password: $(sign_in_says "$page" "$password")"
page=$(phpmyadmin_sign_in not-the-password)
[[ "$page" != *route=/logout* ]] || fail "phpMyAdmin signed in with a wrong password"

step "Backups: MySQL dumps with nothing stopped, and a restore brings back what a migration took"
my() { docker exec "$mysql_db" sh -c 'MYSQL_PWD=$MYSQL_ROOT_PASSWORD exec mysql -uroot -h127.0.0.1 -N -e "$1"' sh "$1"; }
my "CREATE DATABASE shop; CREATE TABLE shop.orders (id INT PRIMARY KEY, item VARCHAR(64)); INSERT INTO shop.orders VALUES (1, 'first'), (2, 'second'); CREATE USER 'shopper'@'%' IDENTIFIED BY 'shopper-password'; GRANT ALL ON shop.* TO 'shopper'@'%'" ||
  fail "can't fill the database"
mysql_volume=$(volume_id data "$catalog_project")
[[ -n "$mysql_volume" ]] || fail "there's no volume data in $catalog_project"
said=$(dw backups preview --volume-id "$mysql_volume" --json) || fail "previewing a backup failed: $said"
jq -e '.method == "BACKUP_METHOD_DUMP" and .database == "MySQL"' <<<"$said" >/dev/null || fail "the preview says: $said"
started=$(docker inspect --format '{{.State.StartedAt}}' "$mysql_db") || fail "docker inspect failed"
backup=$(backup_take "$mysql_volume")
[[ "$(docker inspect --format '{{.State.StartedAt}}' "$mysql_db")" == "$started" ]] || fail "a dump restarted the database"
my "DROP TABLE shop.orders; CREATE DATABASE made_since" || fail "can't change the database"
change=$(backup_plan "$backup" '["db","phpmyadmin"]')
person_apply "$change"
backup_healthy "$mysql_db"
backup_healthy "$phpmyadmin"
[[ "$(my 'SELECT item FROM shop.orders WHERE id = 2')" == second ]] || fail "the orders didn't come back: $(my 'SELECT item FROM shop.orders WHERE id = 2' 2>&1)"
[[ -z "$(my "SHOW DATABASES LIKE 'made\\_since'")" ]] || fail "what was made since the backup is still there"
[[ "$(my "SELECT count(*) FROM mysql.user WHERE user = 'shopper'")" == 1 ]] || fail "the user didn't come back"
page=$(phpmyadmin_sign_in "$password")
[[ "$page" == *route=/logout* ]] || fail "phpMyAdmin can't sign in after a restore: $(sign_in_says "$page" "$password")"
backup_undo "$change"
backup_healthy "$mysql_db"
[[ -n "$(my "SHOW DATABASES LIKE 'made\\_since'")" ]] || fail "undoing the restore didn't bring back what it replaced"

step "Catalog: Nginx Proxy Manager takes the email it was given and the password it wasn't"
catalog_install nginx-proxy-manager --values ADMIN_EMAIL=owner@example.com
proxy=$(catalog_container proxy)
catalog_pinned "$proxy"
catalog_healthy "$proxy"
password=$(catalog_generated "$proxy" INITIAL_ADMIN_PASSWORD)
admin_port=$(catalog_value nginx-proxy-manager ADMIN_PORT)
signed_in=$(curl -sS --max-time 15 -H 'Content-Type: application/json' -d "{\"identity\":\"owner@example.com\",\"secret\":\"$password\"}" \
  "http://127.0.0.1:$admin_port/api/tokens") || fail "Nginx Proxy Manager doesn't answer on port $admin_port"
jq -e '.token | length > 0' <<<"$signed_in" >/dev/null 2>&1 || fail "Nginx Proxy Manager won't sign in with its password: ${signed_in//"$password"/<the password>}"

step "Catalog: T3 Code runs, stays on this server, and its setup page takes the key no one typed"
catalog_install t3code
t3code=$(catalog_container t3code)
catalog_pinned "$t3code"
catalog_healthy "$t3code"
held=$(docker exec "$t3code" df -kP /dev/shm) || fail "df in $t3code failed"
[[ "$(awk '$NF == "/dev/shm" { print $2 }' <<<"$held")" == 1048576 ]] || fail "T3 Code's Chromium needs 1 GiB of /dev/shm, and has: $held"
key=$(catalog_generated "$t3code" T3_SETUP_KEY)
t3_port=$(catalog_value t3code PORT)
bound=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$t3code") || fail "docker inspect $t3code failed"
jq -e '[.[][] | .HostIp] == ["127.0.0.1"]' <<<"$bound" >/dev/null || fail "T3 Code is published past loopback: $bound"
said=$(docker logs "$t3code" 2>&1) || fail "docker logs $t3code failed"
[[ "$said" != *"$key"* ]] || fail "T3 Code's log holds its setup key"
[[ "$said" != *"anonymous volume"* ]] || fail "T3 Code says a folder of its is on an anonymous volume: ${said:0:600}"
answer=$(curl -fsS --max-time 10 "http://127.0.0.1:$t3_port/.well-known/t3/environment") || fail "T3 Code doesn't answer on port $t3_port"
jq -e . <<<"$answer" >/dev/null || fail "T3 Code says what isn't JSON: ${answer:0:300}"
setup_login() {
  curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Accept: application/json' --data-urlencode "key=$1" \
    "http://127.0.0.1:$t3_port/__setup/login"
}
refused=$(setup_login not-the-key) || fail "T3 Code's setup page didn't answer"
[[ "$refused" == 401 ]] || fail "T3 Code's setup page answered $refused to a wrong key, want 401"
taken=$(setup_login "$key") || fail "T3 Code's setup page didn't answer"
[[ "$taken" == 200 ]] || fail "T3 Code's setup page answered $taken to its key, want 200"

step "Catalog: a route reaches T3 Code, which is how a person gets to it from elsewhere"
t3_site=t3.e2e-dockwell.example.net
dw routes create t3 --project t3code --domain "$t3_site" --service t3code --port 8080 --tls off --yes >/dev/null || fail "a route to T3 Code failed"
page=$(curl -fsS --max-time 10 --resolve "$t3_site:80:127.0.0.1" "http://$t3_site/__setup/hello") || fail "http://$t3_site/__setup/hello doesn't answer through Caddy"
[[ "$page" == *t3-setup* ]] || fail "http://$t3_site/__setup/hello answers something else: ${page:0:300}"
# Unlike an app with a first-visitor wizard, it lets in only what it paired.
session=$(curl -fsS --max-time 10 --resolve "$t3_site:80:127.0.0.1" "http://$t3_site/api/auth/session") || fail "http://$t3_site/api/auth/session doesn't answer through Caddy"
jq -e '.authenticated == false and .auth.policy == "remote-reachable"' <<<"$session" >/dev/null || fail "T3 Code reads a visitor with no pairing as: ${session:0:300}"
dw routes delete t3 --project t3code --yes >/dev/null || fail "routes delete t3 failed"

step "Catalog: a tunnel takes a token, and a person approves what it reaches"
# Nothing here can reach Cloudflare's edge, as ADR 0030 says, so it stops at the plan,
# and the one thing a server's own run would read first is its health check's flags.
planned=$(priv env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text TUNNEL_TOKEN=not-a-real-token "$bin" catalog install cloudflared --values TUNNEL_TOKEN --plan-only --json) ||
  fail "planning a tunnel failed"
[[ "$planned" != *not-a-real-token* ]] || fail "the plan holds the tunnel's token"
jq -e '.change.plan.hostAccess == true' <<<"$planned" >/dev/null || fail "a tunnel on the server's own network plans no host access: $planned"
change=$(jq -r .change.id <<<"$planned")
token_refused "$change"
dw changes discard "$change" >/dev/null || fail "discarding the tunnel's plan failed"
entry=$(dw catalog get cloudflared --json) || fail "catalog get failed"
image=$(jq -r '.entry.compose' <<<"$entry" | sed -n 's/^ *image: //p')
[[ "$image" == cloudflare/cloudflared:*@sha256:* ]] || fail "the tunnel's entry names '$image'"
said=$(docker run --rm "$image" tunnel --metrics 127.0.0.1:20241 ready 2>&1) && fail "a tunnel with nothing running reads as ready: $said"
[[ "$said" != *"flag provided but not defined"* && "$said" != *"Incorrect Usage"* ]] || fail "cloudflared doesn't take its health check: $said"

step "Catalog: a newer release reads as one, and upgrading keeps the data, the password, and what a person added and changed"
# An earlier step may have pushed the same image to a registry of its own, so this
# reads Docker Hub's digest by its name.
digest_of() {
  local all digest
  docker pull -q "$1" >/dev/null || fail "docker pull $1 failed"
  all=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$1") || fail "docker image inspect $1 failed"
  digest=$(sed -n '/^nginx@sha256:/{p;q;}' <<<"$all")
  [[ -n "$digest" ]] || fail "$1 has no digest from Docker Hub: $all"
  echo "$digest"
}
release_first=$(digest_of nginx:1.27-alpine)
release_second=$(digest_of nginx:1.28-alpine)
catalog_release() {
  cat >"$catalog_dir/e2e-notes.yaml" <<EOF
id: e2e-notes
name: E2E Notes
summary: Something to upgrade
category: storage
version: "$2"
release: $1
license: AGPL-3.0-only
appLicense: BSD-2-Clause
source:
  repository: https://github.com/Dockwell/dockwell
web:
  service: web
inputs:
  - name: PORT
    label: Port
    type: INPUT_TYPE_PORT
    defaultValue: "18300"
  - name: NOTES_PASSWORD
    label: Password
    type: INPUT_TYPE_SECRET
    generate: 32
compose: |
  services:
    web:
      image: $3
      environment:
        NOTES_PASSWORD: \${NOTES_PASSWORD}
      ports:
        - "\${PORT}:80"
      volumes:
        - data:/data
  volumes:
    data: {}
EOF
}
catalog_release 1 1.27 "$release_first"
catalog_install e2e-notes
notes=$(catalog_container web)
password=$(catalog_generated "$notes" NOTES_PASSWORD)
docker exec "$notes" sh -c 'echo kept >/data/marker' || fail "can't write to the install's volume"
dw services add-port web --project e2e-notes --host-port 18301 --container-port 80 --yes >/dev/null || fail "adding a port to what's installed failed"
# What a person does to what's installed: a service the file doesn't name, and the
# file's own port closed to this server. The whole spec goes back, as an edit sets all of it.
dw services create mine --project e2e-notes --app-image nginx:1.27-alpine --yes >/dev/null || fail "adding a service to what's installed failed"
mine=$(docker inspect --format '{{.Id}}' "$(catalog_container mine)") || fail "docker inspect of the service a person added failed"
listed=$(dw services list --project e2e-notes --environment production --json) || fail "services list failed"
closed=$(jq -c '.services[] | select(.resource.name == "web") |
  {resource: .resource, spec: {service: (.spec | .app.ports |= map(if .hostPort == 18300 then .hostIp = "127.0.0.1" else . end))}}' <<<"$listed") ||
  fail "can't write the edit that closes the port: $listed"
[[ "$closed" == *'"hostIp":"127.0.0.1"'* ]] || fail "the edit that closes the port doesn't, from: $listed"
dw changes propose --summary "Close the app to other hosts" --edits "$closed" --yes >/dev/null || fail "closing the port failed"
installs=$(dw catalog installs --json) || fail "catalog installs failed"
jq -e '.installs[] | select(.project == "e2e-notes") | has("upgrade") | not' <<<"$installs" >/dev/null || fail "the newest release offers an upgrade: $installs"
if said=$(dw catalog upgrade e2e-notes --plan-only 2>&1); then fail "upgrading what's up to date planned: $said"; fi
[[ "$said" == *"is up to date"* ]] || fail "upgrading what's up to date said: $said"

catalog_release 2 1.28 "$release_second"
installs=$(dw catalog installs --json) || fail "catalog installs failed"
jq -e '.installs[] | select(.project == "e2e-notes") | .upgrade.version == "1.28"' <<<"$installs" >/dev/null || fail "a newer release doesn't read as one: $installs"
planned=$(dw catalog upgrade e2e-notes --plan-only --json) || fail "planning the upgrade failed"
jq -e '(.change.plan.notes | any(.reason == "backup_first")) and (.change.plan.servers[0].steps | any(.backupFirst != null))' <<<"$planned" >/dev/null ||
  fail "the upgrade's plan doesn't back up first: $planned"
change=$(jq -r .change.id <<<"$planned")
dw changes apply "$change" --yes >/dev/null || fail "applying the upgrade failed"
catalog_runs() {
  local at runs
  at=$(catalog_container web)
  runs=$(docker inspect --format '{{.Config.Image}}' "$at") || fail "docker inspect $at failed"
  [[ "$runs" == "$1" ]] || fail "it runs $runs, want $1"
  [[ "$(docker exec "$at" cat /data/marker)" == kept ]] || fail "what was in its volume didn't stay"
  [[ "$(catalog_env "$at" NOTES_PASSWORD)" == "$password" ]] || fail "its password changed"
  for port in 18300 18301; do
    curl -fsS --max-time 10 -o /dev/null "http://127.0.0.1:$port/" || fail "it doesn't serve on port $port"
  done
  bound=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$at") || fail "docker inspect $at failed"
  jq -e '."80/tcp" | length == 2 and any(.HostPort == "18300" and .HostIp == "127.0.0.1")' <<<"$bound" >/dev/null ||
    fail "the port a person closed reads: $bound"
  [[ "$(docker inspect --format '{{.Id}}' "$(catalog_container mine)")" == "$mine" ]] || fail "the service a person added didn't keep running"
}
catalog_runs "$release_second"
installs=$(dw catalog installs --json) || fail "catalog installs failed"
jq -e '.installs[] | select(.project == "e2e-notes") | (has("upgrade") | not) and .pin.version == "1.28"' <<<"$installs" >/dev/null ||
  fail "after the upgrade the install reads: $installs"

step "Catalog: an upgrade backed up what it could change first"
upgrade_volume=$(volume_id data "$catalog_project")
[[ -n "$upgrade_volume" ]] || fail "the install has no volume data"
backups=$(dw backups list --volume-id "$upgrade_volume" --json) || fail "listing backups failed"
jq -e '.backups | length == 1 and .[0].state == "BACKUP_STATE_DONE" and (.[0].sizeBytes | tonumber > 0)' <<<"$backups" >/dev/null ||
  fail "the upgrade left no backup: $backups"

step "Catalog: undoing an upgrade puts the old version back, with the data as it is"
dw changes undo "$change" --yes >/dev/null || fail "undoing the upgrade failed"
catalog_runs "$release_first"
installs=$(dw catalog installs --json) || fail "catalog installs failed"
jq -e '.installs[] | select(.project == "e2e-notes") | .upgrade.version == "1.28" and .pin.version == "1.27"' <<<"$installs" >/dev/null ||
  fail "after the undo the install reads: $installs"

step "The agent build joins with Docker down, says so from the start, and its server isn't healthy"
made=$(dw token create 2>>"$log") || fail "dockwell token create failed"
ticket=$(awk '$1 == "dockwell" && $2 == "join" { print $3 }' <<<"$made")
[[ -n "$ticket" ]] || fail "dockwell token create printed no ticket: $made"
"$agent" join --data-dir "$agent_data" --name e2e-agent "$ticket" >"$agent_log" 2>&1 || fail "the agent build didn't join"
# A socket nothing listens on is Docker down, as after a reboot where it didn't come up.
DOCKER_HOST=unix:///nonexistent/docker.sock "$agent" run --data-dir "$agent_data" --log-format json >"$agent_log" 2>&1 &
agent_pid=$!
agent_id=""
for _ in $(seq 300); do
  # Under pipefail a grep with nothing to find yet would end the script, without a word.
  agent_id=$(grep '"msg":"server connected"' "$log" | grep '"name":"e2e-agent"' | grep -o '"server":"srv_[^"]*"' | head -1 | cut -d'"' -f4 || true)
  [[ -n "$agent_id" ]] && break
  sleep 0.1
done
[[ -n "$agent_id" ]] || fail "the agent build didn't connect within 30 s with Docker down"
# What the CLI says is read whole, then matched, as a pipe into grep -q could stop it short.
agent_server() { dw servers get "$agent_id" --json 2>>"$log" || true; }
said=""
for _ in $(seq 100); do
  said=$(agent_server)
  [[ "$said" == *'"dockerUnreadableSince"'* ]] && break
  sleep 0.1
done
[[ "$said" == *'"dockerUnreadableSince"'* ]] ||
  fail "with Docker down from its start, the agent build's server doesn't say it can't read Docker: $said"
kill "$agent_pid"
wait "$agent_pid" || true

step "The agent build, with Docker up, says nothing of it, and runs a container"
"$agent" run --data-dir "$agent_data" --log-format json >>"$agent_log" 2>&1 &
agent_pid=$!
joined() {
  local lines
  lines=$(grep '"msg":"server connected"' "$log" | grep '"name":"e2e-agent"' | grep '"build":"agent"') || return 1
  [[ "$lines" == *'"container"'* ]]
}
for _ in $(seq 300); do
  joined && break
  sleep 0.1
done
joined || fail "the agent build didn't connect within 30 s, offering the Docker kinds"
for _ in $(seq 100); do
  said=$(agent_server)
  [[ "$said" == *'"dockerUnreadableSince"'* ]] || break
  sleep 0.1
done
[[ "$said" != *'"dockerUnreadableSince"'* ]] || fail "with Docker up, the agent build's server still says it can't read Docker: $said"
dw containers create e2e-agent-web --on e2e-agent --image nginx:1.27-alpine --yes || fail "containers create on the agent build failed"
owner=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' e2e-agent-web) ||
  fail "Docker runs no container e2e-agent-web"
[[ "$owner" == srv_* ]] || fail "e2e-agent-web is in the Compose project $owner, want its server's, srv_…"
running=$(docker inspect --format '{{.State.Running}}' e2e-agent-web) || fail "docker inspect e2e-agent-web failed"
[[ "$running" == true ]] || fail "e2e-agent-web isn't running"
# The agent build lists what its server has, and the control plane knows its own.
on_agent=$(dw containers list --server e2e-agent --json) || fail "containers list on the agent build failed"
[[ "$on_agent" != *'"e2e-agent-web"'* ]] || fail "the agent build's own container is listed as one Dockwell didn't make: $on_agent"
jq -e '.ownContainers >= 1' <<<"$on_agent" >/dev/null || fail "the list on the agent build doesn't say what it left out: $on_agent"
dw containers delete e2e-agent-web --on e2e-agent --yes || fail "containers delete on the agent build failed"
if docker inspect e2e-agent-web >/dev/null 2>&1; then fail "e2e-agent-web is left after deleting it"; fi

step "One change reaches two servers, each with its own agent"
# Both on this one Docker, so each container has a name of its own. What runs the same
# service on two Dockers stays on the fake engine, as ADR 0021 says.
own_id=$(dw servers list --json | jq -r --arg agent "$agent_id" '[.servers[] | select(.id != $agent) | .id] | first // empty') || fail "servers list failed"
[[ -n "$own_id" ]] || fail "the control server isn't in the list of servers: $(dw servers list --json)"
two_edit() {
  printf '{"resource":{"kind":"container","scope":"%s","name":"%s"},"spec":{"container":{"image":"%s"}}}' "$1" "$2" "$3"
}
both() { dw changes propose --summary "$1" --edits "$(two_edit "$own_id" e2e-two-own "$2")" --edits "$(two_edit "$agent_id" e2e-two-agent "$2")" "${@:3}"; }
both "Run one on each server" nginx:1.27-alpine --yes || fail "a change on two servers failed"
image_of() { docker inspect --format '{{.Config.Image}}' "$1"; }
for name in e2e-two-own e2e-two-agent; do
  [[ "$(image_of "$name")" == nginx:1.27-alpine ]] || fail "$name runs $(image_of "$name")"
done

step "With a server offline, a change that needs it is refused by name, and touches nothing, until it's back"
kill "$agent_pid"
wait "$agent_pid" || true
connection() { agent_server | jq -r '.server.connection // empty'; }
state=""
for _ in $(seq 100); do
  state=$(connection)
  [[ -n "$state" && "$state" != CONNECTION_CONNECTED ]] && break
  sleep 0.1
done
[[ -n "$state" && "$state" != CONNECTION_CONNECTED ]] || fail "with its agent stopped, the server reads '$state'"
planned=$(both "Move both" nginx:1.28-alpine --plan-only --json 2>/dev/null) || fail "planning with a server offline failed"
jq -e '.change.plan.servers | any(.offline == true)' <<<"$planned" >/dev/null || fail "the plan doesn't say a server is offline: $planned"
change=$(jq -r .change.id <<<"$planned")
if said=$(dw changes apply "$change" --yes 2>&1); then fail "a change that needs an offline server applied: $said"; fi
[[ "$said" == *"e2e-agent isn't connected"* ]] || fail "a change that needs an offline server was refused saying: $said"
for name in e2e-two-own e2e-two-agent; do
  [[ "$(image_of "$name")" == nginx:1.27-alpine ]] || fail "a refused change touched $name, which runs $(image_of "$name")"
done
dw changes discard "$change" >/dev/null || fail "discarding the change failed"
"$agent" run --data-dir "$agent_data" --log-format json >>"$agent_log" 2>&1 &
agent_pid=$!
for _ in $(seq 300); do
  state=$(connection)
  [[ "$state" == CONNECTION_CONNECTED ]] && break
  sleep 0.1
done
[[ "$state" == CONNECTION_CONNECTED ]] || fail "the agent didn't come back within 30 s: it reads '$state'"
both "Move both" nginx:1.28-alpine --yes || fail "the change failed with its server back"
for name in e2e-two-own e2e-two-agent; do
  [[ "$(image_of "$name")" == nginx:1.28-alpine ]] || fail "with its server back, $name runs $(image_of "$name")"
done
dw containers delete e2e-two-own --on "$own_id" --yes || fail "deleting e2e-two-own failed"
dw containers delete e2e-two-agent --on "$agent_id" --yes || fail "deleting e2e-two-agent failed"

echo "docker-e2e: ok"
