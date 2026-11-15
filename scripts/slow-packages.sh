#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Warns, without failing, of each package whose tests took longer than a limit, from
# go test's output, so a package nearing the Go job's time shows before it runs out
# (#197):
#
#   scripts/slow-packages.sh <go test output> [seconds, 300 by default]
set -euo pipefail

out=${1:?usage: scripts/slow-packages.sh <go test output> [seconds]}
limit=${2:-300}
awk -v limit="$limit" '
  ($1 == "ok" || $1 == "FAIL") && $3 ~ /^[0-9.]+s$/ {
    took = $3
    sub(/s$/, "", took)
    if (took + 0 > limit + 0)
      printf "::warning title=Slow tests::%s took %d s, past %d s, which leaves the Go job less of its time. Find its slowest tests with go test -json, as #197 did.\n", $2, took, limit
  }' "$out"
