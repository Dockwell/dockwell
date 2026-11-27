#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
full=$(realpath "${1:?usage: scripts/small-host.sh path/to/dockwell [results.json]}")
out=$(realpath -m "${2:-$root/bin/small-host.json}")
rm -f -- "$out"
work=$(mktemp -d)
# shellcheck disable=SC2329 # trap calls it
cleanup() { rm -rf -- "$work"; }
trap cleanup EXIT

cd "$root"
for tool in claim fakedocker gitserve; do
  CGO_ENABLED=0 go build -o "$work/$tool" "./scripts/$tool"
done
caddy=${DOCKWELL_TEST_CADDY:-}
if [[ -z $caddy ]]; then
  caddy=$(scripts/ingress-tools.sh "$root/bin/ingress-tools" caddy)/caddy
fi
caddy=$(realpath "$caddy")
args=(python3 "$root/scripts/small-host.py" "$work" "$full" "$caddy" "$out" "${SMALL_HOST_SECONDS:-180}")

# Probe in a disposable scope before starting the workload. If the actual run
# fails under a cap, preserve the failure. Never retry it with the cap removed.
# shellcheck disable=SC2016 # expand in the child scope
probe=(sh -ec 'group=$(sed -n "s/^0:://p" /proc/self/cgroup); test -n "$group"; test "$(cat "/sys/fs/cgroup$group/memory.max")" = 1073741824; test "$(cat "/sys/fs/cgroup$group/memory.swap.max")" = 0; test -r "/sys/fs/cgroup$group/memory.events"')
scope=(systemd-run --scope --quiet --property=MemoryMax=1G --property=MemorySwapMax=0)
if command -v systemd-run >/dev/null && "${scope[@]}" --user -- "${probe[@]}" >/dev/null 2>&1; then
  "${scope[@]}" --user -- "${args[@]}" cgroup
elif command -v sudo >/dev/null && command -v setpriv >/dev/null &&
  sudo -n "${scope[@]}" -- "${probe[@]}" >/dev/null 2>&1; then
  # A CI runner can create a system scope as root. Keep the workload itself
  # under the calling user, with its files and the same installed-service limit.
  sudo -n "${scope[@]}" -- setpriv --reuid="$(id -u)" --regid="$(id -g)" --init-groups \
    env "PATH=$PATH" "HOME=$HOME" "${args[@]}" cgroup
elif [[ $(id -u) == 0 ]] && command -v systemd-run >/dev/null &&
  "${scope[@]}" -- "${probe[@]}" >/dev/null 2>&1; then
  "${scope[@]}" -- "${args[@]}" cgroup
else
  echo "small-host: measuring only with process PSS. No writable 1 GiB cgroup scope."
  "${args[@]}" sampled
fi
