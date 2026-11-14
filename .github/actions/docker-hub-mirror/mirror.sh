#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Points this runner's Docker daemon at a mirror of Docker Hub (#651):
#
#   .github/actions/docker-hub-mirror/mirror.sh <mirror>
#
# It restarts the daemon once, before the job runs anything on it. A reload would
# read the mirror too, but Docker 28 loses host-gateway's address on one, and then a
# container that maps host-gateway can't start. One small pull with no login, as
# Dockwell's own are, then shows whether the mirror served it: Docker tries the
# mirror first, and says at info level when it goes on to another registry, and why.
#
# A pull that carries a Docker Hub login, as the runner's docker CLI has, goes to
# Docker Hub under that login: the daemon hands the mirror the login too, and the
# mirror refuses one it doesn't know.
#
# Nothing here fails the job, and it ends with the daemon up: on the mirror, or, if
# Docker won't run with it, on the config the job started with.
set -euo pipefail

mirror=${1:?usage: mirror.sh <mirror>}
config=${DOCKER_DAEMON_CONFIG:-/etc/docker/daemon.json}
probe=hello-world:latest

warn() { echo "::warning title=Docker Hub mirror::$*"; }

# write writes the daemon's config as jq's filter $1 makes it from $before.
write() {
  jq --arg mirror "$mirror" "$1" <<<"${before:-"{}"}" | sudo -n tee "$config.new" >/dev/null &&
    sudo -n mv "$config.new" "$config"
}

# says waits until docker info, in the format $1, holds $2, for 30 seconds at most.
says() {
  local _
  for _ in $(seq 150); do
    [[ "$(docker info --format "$1" 2>/dev/null)" == *"$2"* ]] && return 0
    sleep 0.2
  done
  return 1
}

# start starts the daemon on the config as it's written. systemd refuses a fourth
# start within a minute, and the runner's own start may be one, so its count goes
# back to nothing first.
start() {
  sudo -n systemctl reset-failed docker.service docker.socket 2>/dev/null || true
  sudo -n systemctl restart docker
}

# why prints what systemd and the daemon said, for a start that didn't come up.
why() {
  echo "::group::Why Docker didn't come up"
  sudo -n systemctl status docker --no-pager -l 2>&1 | tail -n 20 || true
  sudo -n journalctl -u docker -n 30 --no-pager -o cat 2>&1 || true
  echo "::endgroup::"
}

sudo -n mkdir -p "$(dirname "$config")"
before=$(sudo -n cat "$config" 2>/dev/null) || before=""
# shellcheck disable=SC2016 # $mirror is jq's, from --arg
if ! write '.["registry-mirrors"] = ((.["registry-mirrors"] // []) - [$mirror] + [$mirror])' ||
  ! start || ! says '{{json .RegistryConfig.Mirrors}}' "$mirror"; then
  why
  # Once more as it is, as a start can fail for reasons of its own, then on the
  # config the job started with.
  if start && says '{{json .RegistryConfig.Mirrors}}' "$mirror"; then
    echo "Docker came up with $mirror on a second start."
  else
    write '.' || true
    start || true
    if says '{{.ServerVersion}}' .; then
      warn "Docker didn't run with $mirror as a mirror, so it runs as it did, and this job pulls from Docker Hub."
    else
      why
      warn "Docker didn't come back, with $mirror or without it."
    fi
    exit 0
  fi
fi

start_time=$(date +%s)
had=$(docker image ls -q "$probe")
pulled=true
anonymous=$(mktemp -d)
DOCKER_CONFIG=$anonymous docker pull -q "$probe" >/dev/null || pulled=false
rm -rf "$anonymous"
# Where the daemon went on from the mirror, and why.
fell=$(sudo -n journalctl -u docker --since "@$start_time" --no-pager -o cat 2>/dev/null |
  grep "Attempting next endpoint" || true)
[[ -n "$had" ]] || docker image rm "$probe" >/dev/null 2>&1 || true

if ! $pulled; then
  warn "$probe couldn't be pulled to try $mirror. The daemon still has it as a mirror."
elif [[ -n "$fell" ]]; then
  warn "$mirror didn't serve $probe, so Docker Hub did. This job's pulls may still meet Docker Hub's limit."
  echo "$fell"
elif docker info --format '{{json .DriverStatus}}' 2>/dev/null | grep -q containerd.snapshotter; then
  echo "Docker Hub's images come through $mirror. This daemon's image store doesn't say which registry served $probe."
else
  echo "Docker Hub's images come through $mirror, which served $probe to a pull with no login: Docker tries it first, and went on to no other registry."
fi
# Which registries the runner's docker CLI has logins for, by name alone.
logins=$(jq -r '[(.auths // {} | keys[]), (.credHelpers // {} | keys[])] | unique | join(", ")' \
  "${DOCKER_CONFIG:-$HOME/.docker}/config.json" 2>/dev/null || true)
if [[ "$logins" == *docker.io* ]]; then
  echo "The runner's docker CLI has a login for Docker Hub, so its own pulls, as docker run and compose make, go to Docker Hub under it."
fi
echo "Registry mirrors: $(docker info --format '{{json .RegistryConfig.Mirrors}}' 2>/dev/null)"
