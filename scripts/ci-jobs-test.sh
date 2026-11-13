#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Holds scripts/ci-jobs.sh to examples: the jobs each kind of change picks,
# what a job hears when it asks whether it's needed, how it reads a pull
# request's files from git, and what Install, every job passes and fails.
# Licenses and links runs this on every change, so a rule that stops matching
# fails the pull request that changed it.
#
#   scripts/ci-jobs-test.sh
set -euo pipefail

# In CI these name the step's own files, and the script writes its answers there
# rather than printing them, so an example that forgot to clear them read nothing.
unset GITHUB_OUTPUT GITHUB_STEP_SUMMARY

jobs=$(cd "$(dirname "$0")" && pwd)/ci-jobs.sh
root=$(cd "$(dirname "$0")/.." && pwd)
failed=0
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

all="go docker-go caddy-go docker-e2e catalog-docker store-docker docker-panel logs-load containers-load transfers-load chaos e2e schema panel snapshot budgets install systemd every-leg"
# Go code: every CI job, the budgets, and some install legs.
go="go docker-go caddy-go docker-e2e catalog-docker store-docker docker-panel logs-load containers-load transfers-load chaos e2e schema panel snapshot budgets install systemd"
web="schema panel e2e docker-panel"
install="snapshot install systemd every-leg"

# sorted prints its words sorted, once each.
sorted() { tr ' ' '\n' <<<"$1" | sed '/^$/d' | sort -u | paste -sd ' ' -; }

# picks checks that a change to these paths picks these jobs, in any order.
picks() {
  local want got
  want=$(sorted "$1")
  shift
  got=$(printf '%s\0' "$@" | GITHUB_OUTPUT='' "$jobs" pick | sed -n 's/^jobs=//p' | jq -r 'join(" ")')
  got=$(sorted "$got")
  if [[ $got != "$want" ]]; then
    echo "ci-jobs: $* picks [$got], want [$want]" >&2
    failed=1
  fi
}

picks "" docs/install.md
picks "" CONTRIBUTING.md .github/pull_request_template.md
picks "" docs/adr/0001-record-architecture-decisions.md assets/icon.svg
picks "" scripts/require-green.sh scripts/check-spdx.sh scripts/adrs.sh
# Biome, in the Panel job, checks these wherever they are.
picks "panel" docs/example/data.json
picks "panel" assets/theme.css docs/example/app.tsx docs/example/tool.mjs
picks "$install panel" scripts/install-test/fixture.json
picks "$go" internal/resource/testdata/addresses.json
# biome.json leaves these out.
picks "$install" scripts/sigstore_trusted_root.json
picks "schema panel" design/system/tokens.json
# The release archives carry LICENSE and README.md.
picks "snapshot" LICENSE
picks "snapshot" README.md
picks "$go" internal/control/server.go
picks "$go" internal/cli/apicmd/apicmd.gen.go
picks "$go" internal/cli/api.go
picks "$go" internal/cli/join.go
picks "$go" cmd/dockwell/main.go
picks "$go" proto/dockwell/v1/servers.proto gen/openapi/openapi.yaml
picks "$go" scripts/docker-e2e.sh
picks "$go" scripts/transfers-load.sh
picks "$go" scripts/fakedocker/main.go
picks "go chaos" scripts/chaos/main.go scripts/chaos.sh
picks "go chaos" scripts/chaos/recovery_test.go
picks "chaos" scripts/docker-stops-evidence.sh
picks "$go" scripts/check-panel-size.sh
picks "$go" internal/catalog/catalog.go
# The converter, the record of what each store converts to, and the script that
# installs a sample of each on Docker: Go code, which Other stores on Docker runs.
picks "$go" internal/catalog/convert/rates.json
picks "$go" scripts/store-e2e.sh
# go vet ./... compiles a Go file anywhere.
picks "$go" docs/example/main.go
picks "go docker-go caddy-go" internal/agent/files/stuck_test.go
picks "go docker-go caddy-go" internal/cli/api_test.go cmd/dockwell/build_test.go
picks "$go" internal/agent/files/stuck_test.go internal/docker/dockertest/fake.go
picks "go docker-go caddy-go $web" internal/control/changes_test.go web/src/main.tsx
picks "$go every-leg" go.mod go.sum
picks "$go every-leg" tools/go.mod
picks "$web" web/src/features/apps/AppsPage.tsx
picks "$web" web/src/locales/zh-CN/messages.po packages/ui/src/Button.tsx
picks "$web" pnpm-lock.yaml package.json
# The panel's stylesheet bundles the fonts.
picks "$web" design/system/fonts/HostGrotesk-Variable.woff2
picks "schema panel" design/mocks/apps.dc.html design/system/tokens.json
picks "schema panel" packages/mockview/src/mocks.ts
picks "go docker-e2e catalog-docker e2e" internal/catalog/snapshot/entries/redis.yaml
picks "go docker-e2e catalog-docker e2e" internal/catalog/snapshot/entries/redis.yaml internal/catalog/snapshot/entries/mariadb.yaml
picks "go docker-e2e catalog-docker e2e" internal/catalog/snapshot/entries/media/redis-icon.webp
picks "go docker-e2e catalog-docker e2e" internal/catalog/snapshot/commit
# Service end to end on Docker uninstalls with it.
picks "go containers-load transfers-load docker-e2e snapshot budgets $install" scripts/install.sh
# It runs inside the install legs, under mawk, gawk and busybox awk.
picks "$go $install" scripts/connected.awk
picks "$install" scripts/install-test/run.sh
picks "$install" scripts/sigstore_trusted_root.json
picks "snapshot budgets" scripts/budgets.sh
picks "snapshot budgets $install" .goreleaser.yaml
picks "$all" .github/workflows/ci.yml
picks "$all" .github/workflows/chaos.yml
picks "$all" Makefile
picks "$all" scripts/ci-jobs.sh
picks "$all" .gitignore
picks "$all" newdir/file.txt
picks "$all" docs/install.md .gitattributes
picks "$go" "internal/control/a file with spaces.go"

# A path that's only in the old name of a rename still counts, as git diff
# --no-renames lists both.
picks "$go" docs/moved.txt internal/control/moved.txt

# The last path counts without a NUL after it.
if [[ $(printf 'docs/x.md\0web/src/main.tsx' | GITHUB_OUTPUT='' "$jobs" pick) != *'"panel"'* ]]; then
  echo "ci-jobs: a last path with no NUL after it picks nothing" >&2
  failed=1
fi

all_json=$("$jobs" pick --all | sed -n 's/^jobs=//p')
if [[ $(sorted "$(jq -r 'join(" ")' <<<"$all_json")") != "$(sorted "$all")" ]]; then
  echo "ci-jobs: pick --all picks $all_json, want every job" >&2
  failed=1
fi

# The install legs are the Makefile's images.
images=$(sed -n 's/^install_images="\(.*\)"$/\1/p' "$jobs")
make_images=$(sed -n 's/^INSTALL_TEST_IMAGES ?= //p' "$root/Makefile")
if [[ $images != "$make_images" ]]; then
  echo "ci-jobs: the install legs are [$images], but the Makefile's INSTALL_TEST_IMAGES are [$make_images]. Make them the same." >&2
  failed=1
fi

# output prints what needs writes for these jobs, with what's in the working
# directory's git, or the paths in $CI_JOBS_FILES.
output() {
  # shellcheck disable=SC2086 # the jobs are words
  GITHUB_OUTPUT='' GITHUB_STEP_SUMMARY='' "$jobs" needs $1 | grep '='
}

# asks checks that needs writes these values, among others, for a change to
# these paths.
asks() {
  local want=$1 jobs_asked=$2 got pair
  shift 2
  printf '%s\0' "$@" >"$scratch/paths"
  got=$(CI_JOBS_FILES="$scratch/paths" output "$jobs_asked") || got="a failed run"
  for pair in $want; do
    if ! grep -qxF "$pair" <<<"$got"; then
      echo "ci-jobs: needs $jobs_asked for $* doesn't write $pair: $(paste -sd ' ' <<<"$got")" >&2
      failed=1
    fi
  done
}

some_legs='install-matrix={"include":[{"image":"alpine:3.24","arch":"arm64"},{"image":"almalinux:9","arch":"amd64"}]}'
every_leg='install-matrix={"include":[{"image":"debian:13","arch":"amd64"},{"image":"debian:13","arch":"arm64"},{"image":"ubuntu:26.04","arch":"amd64"},{"image":"ubuntu:26.04","arch":"arm64"},{"image":"almalinux:9","arch":"amd64"},{"image":"almalinux:9","arch":"arm64"},{"image":"alpine:3.24","arch":"amd64"},{"image":"alpine:3.24","arch":"arm64"}]}'

asks "go=false run=false" go web/src/main.tsx
asks "go=true run=true" go internal/control/server.go
asks "panel=true run=true" panel internal/control/server.go
asks "snapshot=true run=true budgets=true install=true systemd=true $some_legs systemd-matrix={\"arch\":[\"arm64\"]}" \
  "snapshot budgets install systemd" internal/control/server.go
asks "snapshot=true run=true budgets=false install=true systemd=true $every_leg systemd-matrix={\"arch\":[\"amd64\",\"arm64\"]}" \
  "snapshot budgets install systemd" scripts/install-test/run.sh
asks "snapshot=true run=true budgets=true install=true systemd=true $every_leg" \
  "snapshot budgets install systemd" internal/control/server.go scripts/install.sh
asks "snapshot=false run=false budgets=false install=false systemd=false jobs=[]" \
  "snapshot budgets install systemd" docs/install.md
asks "snapshot=true run=true install=false jobs=[\"snapshot\"]" "snapshot budgets install systemd" LICENSE
if CI_JOBS_FILES=/dev/null GITHUB_OUTPUT='' "$jobs" needs nosuchjob >/dev/null 2>&1; then
  echo "ci-jobs: needs takes a job no rule names" >&2
  failed=1
fi
if ! GITHUB_EVENT_NAME=push output "systemd" | grep -qxF "systemd-matrix={\"arch\":[\"amd64\",\"arm64\"]}"; then
  echo "ci-jobs: needs on a push to main doesn't run every leg" >&2
  failed=1
fi

# The git path, in a repository of its own: main, a pull request's merge
# commit as GitHub makes it, and clones of it as actions/checkout makes them.
git_in() { git -C "$1" -c user.name=ci-jobs -c user.email=ci-jobs@example.com "${@:2}"; }
origin=$scratch/origin
git init -q "$origin"
git_in "$origin" checkout -q -b main
mkdir -p "$origin/docs" "$origin/web/src" "$origin/internal/x"
echo a >"$origin/docs/a.md"
echo b >"$origin/web/src/main.tsx"
echo c >"$origin/internal/x/data.txt"
git_in "$origin" add -A
git_in "$origin" commit -q -m main
# A pull request that changes the panel and moves a file out of internal/.
git_in "$origin" checkout -q -b feature
echo b2 >"$origin/web/src/main.tsx"
git_in "$origin" mv internal/x/data.txt docs/data.txt
git_in "$origin" add -A
git_in "$origin" commit -q -m feature
# A pull request stacked on it, which changes only docs.
git_in "$origin" checkout -q -b stacked
echo a2 >"$origin/docs/a.md"
git_in "$origin" add -A
git_in "$origin" commit -q -m stacked
# Their merge commits, into main and into feature.
git_in "$origin" checkout -q -b pr-merge main
git_in "$origin" merge -q --no-ff -m merge feature
git_in "$origin" checkout -q -b stacked-merge feature
git_in "$origin" merge -q --no-ff -m merge stacked
git_in "$origin" checkout -q main

# clone_at clones a branch of origin at a depth, as actions/checkout does.
clone_at() {
  rm -rf "$scratch/clone"
  git clone -q --depth "$2" --branch "$1" "file://$origin" "$scratch/clone" 2>/dev/null
}

# git_asks checks what needs go panel writes in the clone, as a pull request.
git_asks() {
  local want=$1 what=$2 got
  got=$(cd "$scratch/clone" && GITHUB_EVENT_NAME=pull_request output "go panel" | grep -E '^(go|panel)=' | paste -sd ' ' -) ||
    got="a failed run"
  if [[ $got != "$want" ]]; then
    echo "ci-jobs: needs go panel on $what writes [$got], want [$want]" >&2
    failed=1
  fi
}

for depth in 1 2; do
  clone_at pr-merge "$depth"
  # The move's old path, in internal/, needs Go, and the panel's change the Panel job.
  git_asks "go=true panel=true" "a merge commit at depth $depth"
done
# Stacked on another pull request, it's compared with main, not its base, so
# the base's changes count too.
for depth in 1 2; do
  clone_at stacked-merge "$depth"
  git_asks "go=true panel=true" "a pull request based on another, at depth $depth"
done
# A clone of main itself, as a pull request that changes nothing, needs nothing.
clone_at main 1
git_asks "go=false panel=false" "a pull request with no changes"
# Without main to compare with, every job runs.
clone_at main 1
CI_JOBS_MAIN=nosuchbranch git_asks "go=true panel=true" "a clone without main"
# A full clone stays full: API schema's checkout reads the history.
rm -rf "$scratch/clone"
git clone -q --branch pr-merge "file://$origin" "$scratch/clone" 2>/dev/null
git_asks "go=true panel=true" "a full clone"
if [[ $(git -C "$scratch/clone" rev-parse --is-shallow-repository) != false ]]; then
  echo "ci-jobs: needs made a full clone shallow, which cuts the history API schema reads" >&2
  failed=1
fi
# Outside a repository, every job runs.
mkdir -p "$scratch/nogit"
got=$(cd "$scratch/nogit" && GIT_CEILING_DIRECTORIES=$scratch GITHUB_EVENT_NAME=pull_request output "go" | grep '^go=') ||
  got="a failed run"
if [[ $got != "go=true" ]]; then
  echo "ci-jobs: needs go outside a repository writes [$got], want [go=true]" >&2
  failed=1
fi

# verdict checks that check passes, or fails, on these results, for what
# Release snapshot picked for these paths.
verdict() {
  local want=$1 needs=$2 picked got=pass
  shift 2
  picked=$(printf '%s\0' "$@" | GITHUB_OUTPUT='' "$jobs" pick | sed -n 's/^jobs=//p')
  NEEDS=$needs JOBS=$picked "$jobs" check >/dev/null || got=fail
  if [[ $got != "$want" ]]; then
    echo "ci-jobs: check with $needs, for $*: $got, want $want" >&2
    failed=1
  fi
}

go_ok='{"snapshot":{"result":"success"},"base":{"result":"success"},"budgets":{"result":"success"},"install":{"result":"success"},"systemd":{"result":"success"},"install-time":{"result":"skipped"}}'
web_ok='{"snapshot":{"result":"success"},"base":{"result":"success"},"budgets":{"result":"skipped"},"install":{"result":"skipped"},"systemd":{"result":"skipped"},"install-time":{"result":"skipped"}}'
verdict pass "$go_ok" internal/control/server.go
verdict pass "$web_ok" web/src/main.tsx
verdict fail "$web_ok" internal/control/server.go
verdict fail '{"snapshot":{"result":"failure"},"base":{"result":"success"},"budgets":{"result":"skipped"}}' internal/control/server.go
verdict fail '{"snapshot":{"result":"failure"},"budgets":{"result":"skipped"}}' docs/install.md
verdict fail '{"snapshot":{"result":"success"},"budgets":{"result":"cancelled"}}' scripts/budgets.sh
verdict pass '{"snapshot":{"result":"success"},"base":{"result":"failure"},"budgets":{"result":"success"}}' scripts/budgets.sh
verdict fail '{"snapshot":{"result":"success"},"budgets":{"result":"success"},"install-time":{"result":"failure"}}' scripts/budgets.sh
# Without what Release snapshot picked, every job must pass.
if NEEDS="$web_ok" JOBS='' "$jobs" check >/dev/null; then
  echo "ci-jobs: check without JOBS lets skipped jobs pass" >&2
  failed=1
fi

if ((failed)); then
  echo "ci-jobs: scripts/ci-jobs.sh doesn't do what these examples say. Fix the rule, or the example if the jobs a change needs have changed." >&2
  exit 1
fi
echo "ci-jobs: every example picks the jobs it should, and every verdict holds."
