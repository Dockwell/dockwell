#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Leaves the files of the public API that the agent's protocol doesn't reach out of
# the agent build (ADR 0093, #622). Go links a generated package whole, and each
# file registers its schema as the program starts, so an agent would otherwise hold
# the descriptors of alerts, members, settings and the rest, which it never sends.
#
# Each file of gen/dockwell/v1 that no file of proto/dockwell/agent/v1 or
# proto/dockwell/resource/v1alpha1, the schemas the agent build links, imports,
# directly, publicly or through another, gets "//go:build !agent || fullapi" below its SPDX line. The
# agent build, -tags agent, leaves it out; the full build keeps it; and the agent's
# own packages' tests, vet and lint, -tags agent,fullapi, keep it too, as two of
# them start a control plane to join. make generate runs this after buf generate.
set -euo pipefail

cd "$(dirname "$0")/.."
tag='//go:build !agent || fullapi'

# The files of dockwell/v1 the agent's schemas import, and what those import.
imports() { sed -En 's|^import (public )?"dockwell/v1/([a-z0-9_]*)\.proto";|\2|p' "$@"; }
mapfile -t todo < <(imports proto/dockwell/agent/v1/*.proto proto/dockwell/resource/v1alpha1/*.proto | sort -u)
declare -A reached=()
while ((${#todo[@]} > 0)); do
  f=${todo[-1]}
  unset 'todo[-1]'
  [[ -n ${reached[$f]+x} ]] && continue
  reached[$f]=1
  mapfile -t more < <(imports "proto/dockwell/v1/$f.proto")
  todo+=("${more[@]}")
done
((${#reached[@]} > 0)) || { echo "agent-api: the agent's protocol imports nothing of dockwell/v1, which can't be right" >&2; exit 1; }

for pb in gen/dockwell/v1/*.pb.go; do
  name=$(basename "$pb" .pb.go)
  [[ -n ${reached[$name]+x} ]] && continue
  grep -qxF "$tag" "$pb" && continue
  # The SPDX line stays first, as scripts/check-spdx.sh reads it there.
  awk -v tag="$tag" 'NR == 1 { print; print ""; print tag; next } { print }' "$pb" >"$pb.tmp"
  mv "$pb.tmp" "$pb"
done
