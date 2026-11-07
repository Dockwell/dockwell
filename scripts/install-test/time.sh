#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Times the install script from start to a running panel on one CPU, and fails
# over the PRD's budget of 60 s on a 1 vCPU VPS. Runs as root on a VM booted
# with systemd, such as a GitHub runner, which it changes for good:
#
#   sudo [DOCKWELL_...=...] scripts/install-test/time.sh <label>
#
# One CPU means a cpuset: system.slice, where dockwell.service runs, and the
# install itself are both held to CPU 0, so neither can use the runner's other
# CPUs. The install downloads cosign from GitHub as a server would; the
# DOCKWELL_ variables it passes on pick the release and how to check it.
set -euo pipefail

label=${1:?usage: time.sh <label>}
budget=60
panel=http://127.0.0.1:7600
repo=$(cd "$(dirname "$0")/../.." && pwd)

systemctl set-property --runtime system.slice AllowedCPUs=0
cpus=$(cat /sys/fs/cgroup/system.slice/cpuset.cpus.effective)
[[ "$cpus" == 0 ]] || {
  echo "time.sh: system.slice can still use CPUs $cpus, so this wouldn't measure one CPU. It needs cgroup v2 with the cpuset controller." >&2
  exit 1
}
inside=$(systemd-run --scope --quiet -p AllowedCPUs=0 -- nproc)
[[ "$inside" == 1 ]] || {
  echo "time.sh: a scope pinned to CPU 0 still sees $inside CPUs." >&2
  exit 1
}

# The service asks nothing beyond this machine, and inherits nothing from here
# (DOCKWELL_TEST, ADR 0034).
mkdir -p /etc/systemd/system/dockwell.service.d
printf '[Service]\nEnvironment=DOCKWELL_TEST=1\n' >/etc/systemd/system/dockwell.service.d/test.conf

start=$(date +%s%N)
systemd-run --scope --quiet -p AllowedCPUs=0 -- sh "$repo/scripts/install.sh"
until curl -fsS --max-time 2 -H 'Accept: text/html' "$panel/" 2>/dev/null | grep -q '<div id="root"></div>'; do
  (($(date +%s%N) - start < 120 * 1000000000)) || {
    echo "time.sh: the panel didn't answer within 120 s of starting the install." >&2
    exit 1
  }
  sleep 0.1
done
end=$(date +%s%N)

seconds=$(awk -v ns="$((end - start))" 'BEGIN { printf "%.1f", ns / 1e9 }')
service_cpus=$(taskset -cp "$(systemctl show -p MainPID --value dockwell)" | sed 's/.*: //')
version=$(/usr/local/bin/dockwell version | awk '$1 == "dockwell" { print $2 }')
result="Within budget"
over=0
if awk -v s="$seconds" -v b="$budget" 'BEGIN { exit !(s >= b) }'; then
  result="Over budget"
  over=1
fi

echo "time.sh: $label, $version: install to running panel in $seconds s on CPU $service_cpus ($result; the budget is $budget s)"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "### Install to running panel, on 1 CPU"
    echo
    echo "| Release | Time | Budget | Result |"
    echo "| --- | ---: | ---: | --- |"
    echo "| $label, $version | $seconds s | $budget s | $result |"
    echo
    echo "From starting the install script to the panel answering, with the install and dockwell.service held to CPU $service_cpus. It includes downloading cosign from GitHub, and the release from ${DOCKWELL_RELEASES_URL:-GitHub}."
  } >>"$GITHUB_STEP_SUMMARY"
fi
if ((over)); then
  echo "time.sh: installing took $seconds s, over the $budget s budget. Compare the steps' timings with an earlier nightly run to find what grew." >&2
fi
exit "$over"
