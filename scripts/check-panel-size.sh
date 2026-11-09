#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Fails when the panel build is over its size limit.
#
#   scripts/check-panel-size.sh [internal/panel/dist/app]
#
# The control plane reads every file of the panel it embeds once at start, to
# hash it (internal/panel), so the build's size is about how much it adds to
# the control plane's idle memory, in file-backed pages. The Memory budgets job
# doesn't run for a change to the panel alone (scripts/ci-jobs.sh), so this
# holds the build instead. It counts what's read: every file but the Brotli and
# gzip copies.
#
# The initial 2.5 MB limit allowed about half as much again as main's build
# on 2026-11-07: 1,673,300 bytes in 163 files. The terminal adds a 392 KB chunk
# for xterm, fitting and search; its complete build on 2026-11-08 is 2,548,822
# bytes in 236 files. Allow another 0.5 MB for that feature. The separate idle
# RSS budgets still apply. Raise this limit when a change is worth what it adds,
# and say why in the pull request.
set -euo pipefail

limit=3000000
dir=${1:-internal/panel/dist/app}

if [[ ! -f $dir/index.html ]]; then
  echo "check-panel-size: $dir has no index.html, so there's no panel build to measure. Run make web first." >&2
  exit 1
fi
read -r size files < <(find "$dir" -type f ! -name '*.br' ! -name '*.gz' -printf '%s\n' | awk '{ s += $1; n++ } END { print s + 0, n + 0 }')
mb() { awk -v b="$1" 'BEGIN { printf "%.2f MB", b / 1e6 }'; }
if ((size > limit)); then
  echo "check-panel-size: the panel build is $(mb "$size") in $files files, over its $(mb "$limit") limit. The control plane reads all of it at start, so its idle memory grows by about as much. Find what grew with the Bundle size table in this job's summary, or raise limit in scripts/check-panel-size.sh if it's worth it, and say why in the pull request." >&2
  exit 1
fi
echo "check-panel-size: the panel build is $(mb "$size") in $files files, within its $(mb "$limit") limit."
if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
  echo "The panel build is $(mb "$size") in $files files, which the control plane reads at start, within its $(mb "$limit") limit." >>"$GITHUB_STEP_SUMMARY"
fi
