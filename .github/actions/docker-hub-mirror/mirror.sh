#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Points this runner's Docker daemon at a mirror of Docker Hub (#651):
#
#   .github/actions/docker-hub-mirror/mirror.sh <mirror>
#
# It restarts the daemon, before the job runs anything on it. A reload would read
# the mirror too, but Docker 28 loses host-gateway's address on one, and then a
# container that maps host-gateway can't start. One small pull, with the daemon's
# debug log on for it alone, then shows which registry served it, in the daemon's
# own words. Nothing here fails the job: without the mirror, its pulls go to
# Docker Hub, as they did before.
set -euo pipefail

mirror=${1:?usage: mirror.sh <mirror>}
config=${DOCKER_DAEMON_CONFIG:-/etc/docker/daemon.json}
probe=hello-world:latest

warn() { echo "::warning title=Docker Hub mirror::$*"; }

# configure writes the daemon's config as jq's filter $1 makes it from $before,
# and restarts the daemon on it.
configure() {
  jq --arg mirror "$mirror" "$1" <<<"${before:-"{}"}" | sudo -n tee "$config.new" >/dev/null &&
    sudo -n mv "$config.new" "$config" &&
    sudo -n systemctl restart docker
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

sudo -n mkdir -p "$(dirname "$config")"
before=$(sudo -n cat "$config" 2>/dev/null) || before=""
# shellcheck disable=SC2016 # $mirror is jq's, from --arg
add='.["registry-mirrors"] = ((.["registry-mirrors"] // []) - [$mirror] + [$mirror])'
if ! configure "$add | .debug = true" || ! says '{{json .RegistryConfig.Mirrors}}' "$mirror" || ! says '{{.Debug}}' true; then
  # A config the daemon didn't take would stop it the next time it starts.
  configure '.' || true
  says '{{.ServerVersion}}' . || true
  warn "Docker didn't take $mirror as a mirror, so this job pulls from Docker Hub, as before."
  sudo -n journalctl -u docker -n 20 --no-pager -o cat || true
  exit 0
fi

start=$(date +%s)
had=$(docker image ls -q "$probe")
pulled=true
docker pull -q "$probe" >/dev/null || pulled=false
name=${probe%%:*}
# What the daemon said of this pull: each registry it tried, and why it went on
# from one.
said=$(sudo -n journalctl -u docker --since "@$start" --no-pager -o cat 2>/dev/null |
  grep -E "Trying to pull $name from|Attempting next endpoint|[Ee]rror.*($name|pull)" || true)
[[ -n "$had" ]] || docker image rm "$probe" >/dev/null 2>&1 || true

# The debug log back as it was, with the mirror kept.
restore="$add | del(.debug)"
if jq -e 'has("debug")' <<<"${before:-"{}"}" >/dev/null; then
  restore=$add
fi
if ! configure "$restore" || ! says '{{json .RegistryConfig.Mirrors}}' "$mirror"; then
  warn "Docker didn't come back with $mirror after its debug log went back as it was."
fi

case "$pulled:$said" in
false:*)
  warn "$probe couldn't be pulled to try $mirror. The daemon still has it as a mirror."
  ;;
*"Trying to pull $name from https://registry-1.docker.io"*)
  warn "$mirror didn't serve $probe, so Docker Hub did. This job's pulls may still meet Docker Hub's limit."
  ;;
*"Trying to pull $name from $mirror"*)
  echo "Docker Hub's images come through $mirror, which served $probe."
  ;;
*)
  echo "Docker Hub's images come through $mirror. The daemon's log doesn't say which registry served $probe."
  ;;
esac
if [[ -n "$said" ]]; then
  echo "::group::What the daemon said of pulling $probe"
  echo "$said"
  echo "::endgroup::"
fi
echo "Registry mirrors: $(docker info --format '{{json .RegistryConfig.Mirrors}}')"
