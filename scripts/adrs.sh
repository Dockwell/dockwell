#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Lists the architecture decision records: each one's number and title, and
# its status up to the first full stop, from the record itself. The records
# are the index, so a pull request that adds one edits no shared list
# (docs/adr/0051-files-that-merge-without-conflicts.md).
#
#   make adrs
set -euo pipefail

dir=$(dirname "$0")/../docs/adr
for f in "$dir"/[0-9][0-9][0-9][0-9]-*.md; do
  # The first sentence of the status, and the records the rest of it links to.
  awk 'FNR == 1 { sub(/^# /, ""); title = $0 }
    /^- Status: / {
      sub(/^- Status: /, ""); links = $0; gsub(/\[|\]\([^)]*\)/, "")
      first = $0; sub(/\. .*/, "", first)
      while (match(links, /\]\([0-9][0-9][0-9][0-9]-/)) {
        n = substr(links, RSTART + 2, 4); links = substr(links, RSTART + 7)
        if (index(first " " seen, n) == 0) { seen = seen (seen == "" ? "" : ", ") n }
      }
      printf "%s  (%s%s)\n", title, first, seen == "" ? "" : "; see " seen; done = 1; exit
    }
    END { if (!done) printf "%s  (no status)\n", title }' "$f"
done
