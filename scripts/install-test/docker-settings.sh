#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Run only on a disposable install-test runner with a real docker.service.
set -euo pipefail

agent=$1
suite=$2
fixture=$(mktemp -d /var/lib/dockwell-systemd-test.XXXXXX)
config=/etc/docker/daemon.json
saved=false
trap 'rm -rf -- "$fixture"' EXIT

run_fixture() {
    local status
    if "$@"; then
        return 0
    else
        status=$?
    fi
    printf 'Docker settings fixture %s failed (exit %d)\n' "$1" "$status" >&2
    return "$status"
}

cleanup() {
    local status=$?
    trap - EXIT
    if [[ $status != 0 ]]; then
        journalctl -u docker.service -n 100 --no-pager >&2 || true
    fi
    if [[ $saved == true ]]; then
        cp -a -- "$fixture/original" "$config"
    else
        rm -f -- "$config"
    fi
    systemctl reset-failed docker.service
    systemctl restart docker.service
    rm -rf -- "$fixture"
    exit "$status"
}

install -m 0755 -- "$agent" "$fixture/dockwell"
install -m 0755 -- "$suite" "$fixture/settings.test"
run_fixture "$fixture/dockwell" version >/dev/null
run_fixture "$fixture/settings.test" -test.list '^TestDockerSettingsReloadOnSystemd$' >/dev/null
if [[ -e $config ]]; then
    cp -a -- "$config" "$fixture/original"
    saved=true
fi
trap cleanup EXIT
mkdir -p /etc/docker
docker pull busybox:1.37
test -z "$(docker ps -aq)"
printf '%s\n' '{"registry-mirrors":[],"live-restore":true}' >"$config"
chmod 0640 "$config"
systemctl reset-failed docker.service
systemctl restart docker.service
DOCKWELL_TEST_DAEMON_SYSTEMD="$fixture/dockwell" run_fixture "$fixture/settings.test" -test.v -test.timeout=8m -test.run 'OnSystemd$'
