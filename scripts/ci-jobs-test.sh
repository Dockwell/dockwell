#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Holds scripts/ci-jobs.sh to examples: the jobs each kind of change picks,
# what a job hears when it asks whether it's needed, and what Install, every
# job passes and fails. Licenses and links runs this on every change, so a rule
# that stops matching fails the pull request that changed it.
#
#   scripts/ci-jobs-test.sh
set -euo pipefail

jobs=$(dirname "$0")/ci-jobs.sh
failed=0

all="go docker-go caddy-go docker-e2e catalog-docker docker-panel logs-load containers-load e2e schema panel snapshot budgets install systemd"
go="go docker-go caddy-go docker-e2e catalog-docker docker-panel logs-load containers-load e2e schema panel snapshot budgets"
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

# picks checks that a change to these paths picks these jobs, in any order.
picks() {
  local want=$1 got
  shift
  got=$(printf '%s\0' "$@" | GITHUB_OUTPUT='' GITHUB_STEP_SUMMARY='' "$jobs" pick | sed -n 's/^jobs=//p' | jq -r 'sort | join(" ")')
  want=$(tr ' ' '\n' <<<"$want" | sed '/^$/d' | sort -u | paste -sd ' ' -)
  if [[ $got != "$want" ]]; then
    echo "ci-jobs: $* picks [$got], want [$want]" >&2
    failed=1
  fi
}

picks "" docs/install.md
picks "" README.md CONTRIBUTING.md .github/pull_request_template.md
picks "" docs/adr/0001-record-architecture-decisions.md LICENSE assets/icon.svg
picks "" scripts/require-green.sh scripts/check-spdx.sh
picks "$go" internal/control/server.go
picks "$go" internal/cli/apicmd/apicmd.gen.go
picks "$go" internal/cli/api.go
picks "$go" proto/dockwell/v1/servers.proto gen/openapi/openapi.yaml
picks "$go" scripts/docker-e2e.sh
picks "$go" scripts/fakedocker/main.go
picks "$go" internal/catalog/catalog.go
picks "go docker-go caddy-go" internal/agent/files/stuck_test.go
picks "go docker-go caddy-go" internal/cli/api_test.go cmd/dockwell/build_test.go
picks "$go" internal/agent/files/stuck_test.go internal/docker/dockertest/fake.go
picks "go docker-go caddy-go schema panel e2e docker-panel" internal/control/changes_test.go web/src/main.tsx
picks "$go snapshot install systemd" internal/cli/join.go
picks "$go snapshot install systemd" go.mod go.sum
picks "$go snapshot install systemd" tools/go.mod
picks "$go snapshot install systemd" cmd/dockwell/main.go
picks "schema panel e2e docker-panel" web/src/features/apps/AppsPage.tsx
picks "schema panel e2e docker-panel" web/src/locales/zh-CN/messages.po packages/ui/src/Button.tsx
picks "schema panel e2e docker-panel" pnpm-lock.yaml package.json
picks "schema panel" design/mocks/apps.dc.html design/system/tokens.json
picks "schema panel" packages/mockview/src/mocks.ts
picks "go docker-e2e catalog-docker e2e" internal/catalog/entries/redis.yaml
picks "go docker-e2e catalog-docker e2e" internal/catalog/entries/redis.yaml internal/catalog/entries/mariadb.yaml
picks "go containers-load snapshot budgets install systemd" scripts/install.sh
picks "snapshot install systemd" scripts/install-test/run.sh
picks "snapshot install systemd" scripts/sigstore_trusted_root.json
picks "snapshot budgets" scripts/budgets.sh
picks "snapshot budgets install systemd" .goreleaser.yaml
picks "$all" .github/workflows/ci.yml
picks "$all" Makefile
picks "$all" scripts/ci-jobs.sh
picks "$all" .gitignore
picks "$all" newdir/file.txt
picks "$all" docs/install.md .gitattributes
picks "$go schema panel e2e docker-panel" internal/control/server.go web/src/main.tsx
picks "$go" "internal/control/a file with spaces.go"

# The last path counts without a NUL after it.
if [[ $(printf 'docs/x.md\0web/src/main.tsx' | GITHUB_OUTPUT='' GITHUB_STEP_SUMMARY='' "$jobs" pick) != *'"panel"'* ]]; then
  echo "ci-jobs: a last path with no NUL after it picks nothing" >&2
  failed=1
fi

# A path that's only in the old name of a rename still counts, as git diff
# --no-renames lists both.
picks "$go" docs/moved.go internal/control/moved.go

all_json=$("$jobs" pick --all | sed -n 's/^jobs=//p')
if [[ $(jq -r 'sort | join(" ")' <<<"$all_json") != "$(tr ' ' '\n' <<<"$all" | sort | paste -sd ' ' -)" ]]; then
  echo "ci-jobs: pick --all picks $all_json, want every job" >&2
  failed=1
fi

# asks checks what needs writes for a change to these paths.
asks() {
  local want=$1 jobs_asked=$2 got
  shift 2
  printf '%s\0' "$@" >"$scratch/paths"
  # shellcheck disable=SC2086 # the jobs are words
  got=$(CI_JOBS_FILES="$scratch/paths" GITHUB_OUTPUT='' GITHUB_STEP_SUMMARY='' "$jobs" needs $jobs_asked | grep '=' | paste -sd ' ' -)
  if [[ $got != "$want" ]]; then
    echo "ci-jobs: needs $jobs_asked for $* writes [$got], want [$want]" >&2
    failed=1
  fi
}

asks "go=false run=false" go web/src/main.tsx
asks "go=true run=true" go internal/control/server.go
asks "panel=true run=true" panel internal/control/server.go
asks "snapshot=true run=true budgets=true install=false systemd=false" "snapshot budgets install systemd" internal/control/server.go
asks "snapshot=true run=true budgets=false install=true systemd=true" "snapshot budgets install systemd" scripts/install-test/run.sh
asks "snapshot=false run=false budgets=false install=false systemd=false" "snapshot budgets install systemd" docs/install.md
if CI_JOBS_FILES=/dev/null GITHUB_OUTPUT='' "$jobs" needs nosuchjob >/dev/null 2>&1; then
  echo "ci-jobs: needs takes a job no rule names" >&2
  failed=1
fi
if [[ $(GITHUB_EVENT_NAME=push GITHUB_OUTPUT='' GITHUB_STEP_SUMMARY='' "$jobs" needs systemd | grep '=' | paste -sd ' ' -) != "systemd=true run=true" ]]; then
  echo "ci-jobs: needs on a push to main doesn't run every job" >&2
  failed=1
fi

# verdict checks that check passes, or fails, on these results for a change to
# these paths.
verdict() {
  local want=$1 needs=$2 got=pass
  shift 2
  printf '%s\0' "$@" >"$scratch/paths"
  CI_JOBS_FILES="$scratch/paths" NEEDS=$needs "$jobs" check >/dev/null || got=fail
  if [[ $got != "$want" ]]; then
    echo "ci-jobs: check with $needs, for $*: $got, want $want" >&2
    failed=1
  fi
}

go_ok='{"snapshot":{"result":"success"},"base":{"result":"success"},"budgets":{"result":"success"},"install":{"result":"skipped"},"systemd":{"result":"skipped"},"install-time":{"result":"skipped"}}'
verdict pass "$go_ok" internal/control/server.go
verdict fail "$go_ok" internal/cli/run.go
verdict pass '{"snapshot":{"result":"success"},"base":{"result":"success"},"budgets":{"result":"skipped"},"install":{"result":"skipped"},"systemd":{"result":"skipped"}}' web/src/main.tsx
verdict fail '{"snapshot":{"result":"failure"},"base":{"result":"success"},"budgets":{"result":"skipped"}}' internal/control/server.go
verdict fail '{"snapshot":{"result":"failure"},"budgets":{"result":"skipped"}}' docs/install.md
verdict fail '{"snapshot":{"result":"success"},"budgets":{"result":"cancelled"}}' internal/control/server.go
verdict pass '{"snapshot":{"result":"success"},"base":{"result":"failure"},"budgets":{"result":"success"}}' internal/control/server.go
verdict fail '{"snapshot":{"result":"success"},"budgets":{"result":"success"},"install-time":{"result":"failure"}}' internal/control/server.go

if ((failed)); then
  echo "ci-jobs: scripts/ci-jobs.sh doesn't pick what these examples say. Fix the rule, or the example if the jobs a change needs have changed." >&2
  exit 1
fi
echo "ci-jobs: every example picks the jobs it should, and every verdict holds."
