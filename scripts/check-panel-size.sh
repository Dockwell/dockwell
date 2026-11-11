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
# Catalog app pages add local project logos and previews, with their source
# notices, so browsing works offline and makes no third-party image requests
# (ADR 0057). They are held apart from the code: each set of apps adds about
# 35 KB an app, which would otherwise raise one limit a set at a time and hide
# what the code itself gained. On 2026-11-10, with 32 apps, the code is 3.01 MB
# and the media 1.60 MB. Allow the code 3.25 MB and the media 2.25 MB. Both are
# read at start, so the idle RSS budgets still hold their sum.
# On 2026-11-11 about thirty features landed (roles and invites, the panel's
# domain, databases, firewall, history, drills, runtimes and more), and with
# runtimes (#358) the code reached 3.25 MB. A control plane with its agent idled
# at 66.2 MB of its 250 MB budget, so allow the code 3.75 MB.
set -euo pipefail

code_limit=3750000
media_limit=2250000
dir=${1:-internal/panel/dist/app}
media=catalog-media

if [[ ! -f $dir/index.html ]]; then
  echo "check-panel-size: $dir has no index.html, so there's no panel build to measure. Run make web first." >&2
  exit 1
fi
# The size and count of the files under a folder that are read at start.
measure() { find "$@" -type f ! -name '*.br' ! -name '*.gz' -printf '%s\n' | awk '{ s += $1; n++ } END { print s + 0, n + 0 }'; }
read -r code code_files < <(measure "$dir" -path "$dir/$media" -prune -o)
read -r art art_files < <(if [[ -d $dir/$media ]]; then measure "$dir/$media"; else echo 0 0; fi)
mb() { awk -v b="$1" 'BEGIN { printf "%.2f MB", b / 1e6 }'; }
failed=0
if ((code > code_limit)); then
  echo "check-panel-size: the panel's code is $(mb "$code") in $code_files files, over its $(mb "$code_limit") limit. The control plane reads all of it at start, so its idle memory grows by about as much. Find what grew with the Bundle size table in this job's summary, or raise code_limit in scripts/check-panel-size.sh if it's worth it, and say why in the pull request." >&2
  failed=1
fi
if ((art > media_limit)); then
  echo "check-panel-size: the catalog's bundled artwork is $(mb "$art") in $art_files files, over its $(mb "$media_limit") limit. The control plane reads all of it at start too. Check each new image is WebP at the size web/public/catalog-media/README.md gives and that no notice is kept twice, or raise media_limit in scripts/check-panel-size.sh, and say why in the pull request." >&2
  failed=1
fi
if ((failed)); then
  exit 1
fi
said="the panel's code is $(mb "$code") in $code_files files, within its $(mb "$code_limit") limit, and the catalog's artwork $(mb "$art") in $art_files files, within its $(mb "$media_limit") limit"
echo "check-panel-size: $said."
if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
  echo "The control plane reads both at start: $said." >>"$GITHUB_STEP_SUMMARY"
fi
