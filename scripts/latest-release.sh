#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Prints the tag of a repository's latest published release, or nothing when it
# has none yet. Any other failure to find out, such as a GitHub API error or a
# token that can't see the repository, fails instead of reading as no release.
#
#   scripts/latest-release.sh [<owner>/<repo>]
#
# The repository defaults to $GITHUB_REPOSITORY. Needs gh, with GH_TOKEN in CI.
set -euo pipefail

repo=${1:-${GITHUB_REPOSITORY:?usage: latest-release.sh <owner>/<repo>}}
err=$(mktemp)
trap 'rm -f "$err"' EXIT

if tag=$(gh api "repos/$repo/releases/latest" --jq .tag_name 2>"$err"); then
  printf '%s\n' "$tag"
  exit 0
fi
# GitHub answers 404 for the latest release of a repository that has none, and
# also for a repository the token can't see. Only the first means no release.
if grep -q '(HTTP 404)' "$err" && gh api "repos/$repo" --silent 2>>"$err"; then
  exit 0
fi
echo "latest-release.sh: can't find the latest release of $repo, so nothing was checked against it. GitHub said: $(tr '\n' ' ' <"$err")Check that GH_TOKEN can read the repository and that GitHub is up, then run this again." >&2
exit 1
