#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Fails when what a browser downloads to open the panel is over its limit.
#
#   scripts/check-panel-size.sh [internal/panel/dist/app]
#
# Since #619 it holds the panel's first load: index.html and everything it
# loads at once (its scripts, modules it preloads, styles and fonts), as a
# browser gets them, gzipped where the build has a .gz. The history below is
# of the limit it held before, on the whole build.
#
# Before #619, the control plane read every file of the panel it embeds once at
# start, to hash it (internal/panel), so the build's size was about how much it
# added to the control plane's idle memory, in file-backed pages. The Memory
# budgets job doesn't run for a change to the panel alone (scripts/ci-jobs.sh),
# so this held the build instead: every file but the Brotli and gzip copies.
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
# On 2026-11-12 the catalog's artwork moved out of the panel, into Dockwell/catalog
# and the binary's snapshot of it, which the control plane serves from (ADR 0077),
# so the panel holds code alone. The catalog's own checks hold its images.
# On 2026-11-14 main sat at 3.75 MB, and private routes (#606) tipped it over. A
# control plane with its agent idled at about 75 MB of its 250 MB budget, as #601's
# memory comment measured on 2026-11-13, so allow the code 4.25 MB. #619 is the
# real fix: the control plane stops hashing every file at start, so the panel's
# size stops counting toward its idle memory.
set -euo pipefail

# On 2026-11-14, after #619, the control plane reads no panel file as it starts: an asset's
# name is its ETag, the rest are hashed on their first request, and whether to send a .br or
# .gz is decided by the build. So the build's size no longer counts toward idle memory, and
# this holds what someone waits for instead. The first load was 391,593 bytes gzipped in 90
# files (the whole build 3.93 MB in 339 files): allow it 500 KB, about a quarter more.
first_load_limit=500000
dir=${1:-internal/panel/dist/app}

if [[ ! -f $dir/index.html ]]; then
  echo "check-panel-size: $dir has no index.html, so there's no panel build to measure. Run make web first." >&2
  exit 1
fi
mb() { awk -v b="$1" 'BEGIN { printf "%.2f MB", b / 1e6 }'; }
kb() { awk -v b="$1" 'BEGIN { printf "%.0f KB", b / 1e3 }'; }
# Each file index.html names at its root (src= and href=), as the browser fetches it: its .gz
# where the build wrote one, which is where gzip makes it smaller.
first_load=0
first_files=0
for f in index.html $(grep -oE '(src|href)="/[^"]+"' "$dir/index.html" | sed -E 's/^(src|href)="\///; s/"$//' | sort -u); do
  [[ -f $dir/$f ]] || continue
  if [[ -f $dir/$f.gz ]]; then size=$(stat -c %s "$dir/$f.gz"); else size=$(stat -c %s "$dir/$f"); fi
  first_load=$((first_load + size))
  first_files=$((first_files + 1))
done
read -r code code_files < <(find "$dir" -type f ! -name '*.br' ! -name '*.gz' -printf '%s\n' | awk '{ s += $1; n++ } END { print s + 0, n + 0 }')
if ((first_load > first_load_limit)); then
  echo "check-panel-size: opening the panel downloads $(kb "$first_load") gzipped in $first_files files, over its $(kb "$first_load_limit") limit. Someone opening it waits for all of it. Find what grew with the Bundle size table in this job's summary, load it later with a dynamic import, or raise first_load_limit in scripts/check-panel-size.sh if it's worth it, and say why in the pull request." >&2
  exit 1
fi
said="opening the panel downloads $(kb "$first_load") gzipped in $first_files files, within its $(kb "$first_load_limit") limit; the whole build is $(mb "$code") in $code_files files"
echo "check-panel-size: $said."
if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
  echo "Panel size: $said." >>"$GITHUB_STEP_SUMMARY"
fi
