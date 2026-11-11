#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Run the chaos harness from the repository's root. See docs/testing/chaos.md.
set -euo pipefail
export DOCKWELL_TEST=1

if [[ $# != 2 ]]; then
  echo "usage: scripts/chaos.sh path/to/dockwell path/to/dockwell-agent" >&2
  exit 2
fi
go build -o bin/chaos ./scripts/chaos
if [[ ${CHAOS_FAKE_DOCKER:-} == 1 ]]; then
  go build -o bin/fakedocker ./scripts/fakedocker
  export CHAOS_FAKE_DOCKER="$PWD/bin/fakedocker"
fi
exec bin/chaos "$1" "$2"
