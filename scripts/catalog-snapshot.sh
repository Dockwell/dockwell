#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Writes Dockwell/catalog's entries, and the images and notices beside them, at
# the commit internal/catalog/snapshot/commit names, into the snapshot the binary
# embeds as the fallback when the repository can't be reached (ADR 0077).
#
#   scripts/catalog-snapshot.sh            refresh the snapshot from its commit
#   scripts/catalog-snapshot.sh <commit>   move it to another commit first
#   scripts/catalog-snapshot.sh --check    fail if it isn't what its commit holds
#
# DOCKWELL_CATALOG_REPO names another copy of the repository, such as a local
# clone, for a change that isn't on GitHub yet.
set -euo pipefail

repo=${DOCKWELL_CATALOG_REPO:-https://github.com/Dockwell/catalog}
dir=internal/catalog/snapshot
check=0
if [[ ${1:-} == --check ]]; then
  check=1
elif [[ -n ${1:-} ]]; then
  [[ $1 =~ ^[0-9a-f]{40}$ ]] || { echo "catalog-snapshot: $1 isn't a commit. Name one in full, as 40 hex digits." >&2; exit 1; }
  echo "$1" >"$dir/commit"
fi
commit=$(tr -d '[:space:]' <"$dir/commit")
[[ $commit =~ ^[0-9a-f]{40}$ ]] || { echo "catalog-snapshot: $dir/commit names no commit. Put one of Dockwell/catalog there, in full." >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git -c init.defaultBranch=main init -q "$work/catalog"
git -C "$work/catalog" fetch -q --depth 1 "$repo" "$commit"
git -C "$work/catalog" -c advice.detachedHead=false checkout -q FETCH_HEAD
[[ -d $work/catalog/entries ]] || { echo "catalog-snapshot: $commit of $repo has no folder entries." >&2; exit 1; }

if ((check)); then
  if ! diff -r "$work/catalog/entries" "$dir/entries" >/dev/null; then
    diff -rq "$work/catalog/entries" "$dir/entries" >&2 || true
    echo "catalog-snapshot: $dir/entries isn't what $commit of Dockwell/catalog holds. Run make catalog-snapshot, and commit what it writes." >&2
    exit 1
  fi
  echo "catalog-snapshot: $dir/entries is Dockwell/catalog at $commit."
  exit 0
fi
rm -rf "$dir/entries"
cp -R "$work/catalog/entries" "$dir/entries"
echo "catalog-snapshot: wrote Dockwell/catalog's entries at $commit into $dir/entries."
