#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Checks the list of Cloudflare's ranges the agent builds in against what Cloudflare
# publishes now. A route that names the cloudflare preset believes and answers these
# alone, so a range it lacks means those visitors get a 404 until a release has it,
# and one it keeps may be someone else's. .github/workflows/cloudflare-ranges.yml runs
# this nightly, and on pull requests that change the list. It needs curl.
#
#   scripts/check-cloudflare-ranges.sh
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
source_file=internal/ingress/trust.go
ours=$(mktemp)
theirs=$(mktemp)
trap 'rm -f "$ours" "$theirs"' EXIT

# Each quoted range between cloudflareRanges' braces, one to a line.
awk '/^var cloudflareRanges = /{ in_list = 1; next } in_list && /^}/{ exit } in_list' "$source_file" |
  grep -oE '"[^"]+"' | tr -d '"' | sort >"$ours"
[[ -s "$ours" ]] || {
  echo "check-cloudflare-ranges: found no ranges in $source_file, so this checked nothing. Look for cloudflareRanges there" >&2
  exit 1
}
for list in ips-v4 ips-v6; do
  curl -fsSL --retry 3 "https://www.cloudflare.com/$list"
  echo
done | grep -v '^$' | sort >"$theirs"
[[ -s "$theirs" ]] || {
  echo "check-cloudflare-ranges: Cloudflare published no ranges, so this checked nothing. See https://www.cloudflare.com/ips/" >&2
  exit 1
}
if ! diff -u "$ours" "$theirs"; then
  echo "check-cloudflare-ranges: the list in $source_file isn't what Cloudflare publishes now, above, so a route that names the cloudflare preset doesn't answer or believe what it should. Update cloudflareRanges there, and its date, from https://www.cloudflare.com/ips/" >&2
  exit 1
fi
echo "check-cloudflare-ranges: $(wc -l <"$ours") ranges, as Cloudflare publishes them"
