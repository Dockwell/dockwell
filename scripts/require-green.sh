#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Fails unless a commit is on origin/main and every required CI check passed
# on it. The release workflow runs this before building a tag, so only green
# commits from main are ever signed and published. It waits for checks that
# are still running, for up to 20 minutes.
#
#   scripts/require-green.sh <commit>
#
# Needs GH_TOKEN (checks: read) and a checkout with origin/main fetched.
set -euo pipefail

sha=${1:?usage: scripts/require-green.sh <commit>}
repo=${GITHUB_REPOSITORY:-Dockwell/dockwell}
required=("Go" "API schema" "Licenses and links" "Panel" "Chaos on Docker")
deadline=$((SECONDS + 20 * 60))

if ! git merge-base --is-ancestor "$sha" origin/main; then
  echo "::error::$sha is not on main. Release tags must point at a commit on main; tag one from there." >&2
  exit 1
fi

while :; do
  runs=$(gh api --paginate "repos/$repo/commits/$sha/check-runs" \
    --jq '.check_runs[] | [.name, .status, (.conclusion // "")] | @tsv')
  pending=()
  for check in "${required[@]}"; do
    # The latest run of each check wins.
    line=$(awk -F'\t' -v n="$check" '$1 == n' <<<"$runs" | head -1)
    if [[ -z "$line" ]]; then
      pending+=("$check (not started)")
      continue
    fi
    IFS=$'\t' read -r _ status conclusion <<<"$line"
    if [[ "$status" != "completed" ]]; then
      pending+=("$check ($status)")
    elif [[ "$conclusion" != "success" ]]; then
      echo "::error::CI check \"$check\" is $conclusion on $sha. Fix main, then tag the fixed commit." >&2
      exit 1
    fi
  done
  if ((${#pending[@]} == 0)); then
    echo "Every required check passed on $sha."
    exit 0
  fi
  if ((SECONDS >= deadline)); then
    echo "::error::CI didn't finish on $sha within 20 min: ${pending[*]}. Re-run this workflow once it has." >&2
    exit 1
  fi
  echo "Waiting for CI on $sha: ${pending[*]}"
  sleep 20
done
