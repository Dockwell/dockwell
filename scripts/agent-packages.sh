#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Prints the packages of this module that the agent build links, as ./paths,
# one per line. make lint and make test vet, lint and test them again with
# -tags agent, as CI does, since the agent build compiles them with it
# (docs/adr/0023-a-full-build-and-an-agent-build.md). The rest of the module
# isn't in the agent build, so it builds the same either way.
#
#   scripts/agent-packages.sh
set -euo pipefail

cd "$(dirname "$0")/.."
go list -deps -tags agent -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/dockwell |
  sed -n 's|^github.com/dockwell/dockwell/|./|p'
