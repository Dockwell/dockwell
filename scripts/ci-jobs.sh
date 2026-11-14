#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Which jobs of the CI and Install workflows a pull request needs, from the
# files it changes (docs/adr/0050-ci-runs-what-a-change-needs.md).
#
#   scripts/ci-jobs.sh needs <job>...    in a job: whether this change needs it
#   scripts/ci-jobs.sh check             in Install, every job: whether it all passed
#   scripts/ci-jobs.sh pick [--all]      NUL-separated paths in, jobs=<JSON> out
#
# A job's ID is its key in the workflow. A pull request's files are where its
# merge commit's tree differs from main's, fetched as the job runs: whatever
# its base, and listing more than it changed when main has moved since, never
# less. A push to main, a nightly run and anything but a pull request need
# every job, and so does a pull request when main can't be fetched. So does a
# change to a workflow, the Makefile or this script, or a path no rule here
# knows, so a new kind of file is never left untested.
#
# needs writes run=true or false for the first job, <job>=true or false for
# each, and jobs=<JSON> for all it picked, to $GITHUB_OUTPUT, and says why in
# $GITHUB_STEP_SUMMARY. Asked about install or systemd, it also writes the
# matrix of legs each runs, as install-matrix and systemd-matrix. Each CI job
# runs it first and skips its other steps when it isn't needed, rather than
# waiting for a job that picks: a job that waits for another goes to the back
# of the queue when that one ends, and the queue is where a push spends most
# of its time.
#
# check is Install, every job, the check a branch rule requires of the Install
# workflow, whose install legs are a matrix. It fails unless every job in JOBS,
# what Release snapshot picked, passed, and every other job passed or was
# skipped: a branch rule counts a skipped job as passing. Build of main may
# fail, as it only adds a comparison. NEEDS is the needs context, as JSON.
#
# scripts/ci-jobs-test.sh holds all three to examples, in Licenses and links.
set -euo pipefail
shopt -s inherit_errexit

# Every job the workflows gate on what changed, in the order they're
# listed. Licenses and links and Install, every job always run, and Build of
# main runs on every pull request, as it can't run this before it builds.
# every-leg isn't a job: it runs every install leg, not only some.
ci_jobs="go docker-go caddy-go docker-e2e catalog-docker store-docker docker-panel logs-load containers-load transfers-load chaos e2e schema panel swarm-docker"
install_jobs="snapshot budgets install systemd every-leg"
every_job="$ci_jobs $install_jobs"

# What each job is called in its workflow.
declare -A names=(
  [go]="Go" [docker-go]="Go on Docker" [caddy-go]="Go on Caddy" [docker-e2e]="Service end to end on Docker"
  [catalog-docker]="Catalog on Docker" [store-docker]="Other stores on Docker" [docker-panel]="Panel end to end on Docker" [logs-load]="Log load"
  [containers-load]="Hundreds of containers" [transfers-load]="Transfer load" [e2e]="Panel end to end" [schema]="API schema" [panel]="Panel"
  [chaos]="Chaos on Docker" [swarm-docker]="Swarm import on Docker"
  [snapshot]="Release snapshot" [budgets]="Memory budgets"
  [install]="Install on each image" [systemd]="Install with systemd" [every-leg]="every install leg"
)

# The install legs: these images, as the Makefile's INSTALL_TEST_IMAGES has
# them, on both architectures, and systemd on both. For Go code, which the
# install script runs but which isn't the script or the release, three of
# them: busybox, musl and OpenRC on arm64, an RPM distribution on amd64, and
# systemd on arm64. Each leg takes about a minute.
install_images="debian:13 ubuntu:26.04 almalinux:9 alpine:3.24"
arches="amd64 arm64"
some_install_legs='[{"image":"alpine:3.24","arch":"arm64"},{"image":"almalinux:9","arch":"amd64"}]'
some_systemd_arches='["arm64"]'

# Go code, or what the Go tests, the load tests and the end-to-end jobs read or
# run. The idle memory budgets move with any of it, and some install legs run,
# as the install script runs the binary's run, join, token, claim and role,
# which reach most of the control plane and the agent.
go="${ci_jobs% swarm-docker} snapshot budgets install systemd"
# What makes a Swarm-run app again, or reads one (ADR 0089): the panels' reader and
# stopper, the import and the take-on it reads through, Docker's client, and what
# carries a panel's step from the plan to the agent and back. Only
# these run Swarm import on Docker, as other Go code doesn't reach a Swarm.
swarm="$go swarm-docker"
# The panel and its packages: their lint and tests, the generated code, and the
# end-to-end jobs that drive the panel. Only the full build embeds the panel,
# and it reads each file once at start, so the budgets don't run for it: the
# Panel job holds the build to a size instead.
web="schema panel e2e docker-panel"
# The design system and the mock viewer: the tokens the panel is generated from
# and the viewer's tests, in the Panel and API schema jobs.
design="schema panel"
# A catalog entry: the catalog's Go tests, which hold every entry to its rules,
# the apps tried on real Docker, and the panel's catalog flow. No agent links
# the catalog.
catalog="go docker-e2e catalog-docker e2e"
# A Go test: the Go tests, and the ones on real Docker and Caddy. A test isn't
# in either build, so the binaries, and what runs them, don't change.
gotest="go docker-go caddy-go"
# The install script, its tests, and the release it installs: every leg.
install="snapshot install systemd every-leg"
budgets="snapshot budgets"

# biome_checks says whether Biome, which the Panel job runs over the whole
# repository, checks the path: what biome.json includes, by extension, less what
# it leaves out.
biome_checks() {
  case $1 in
  gen/* | */gen/* | *.gen.ts | design/* | internal/panel/dist/* | packages/ui/src/tokens.ts | \
    packages/ui/src/styles/tokens.css | scripts/sigstore_trusted_root.json) return 1 ;;
  *.json | *.jsonc | *.js | *.jsx | *.mjs | *.cjs | *.ts | *.tsx | *.mts | *.cts | *.css) return 0 ;;
  *) return 1 ;;
  esac
}

# jobs_for prints the jobs that a change to the path needs. The first pattern
# that matches wins, and * matches across slashes.
jobs_for() {
  case $1 in
  .github/workflows/* | .github/actions/* | Makefile | scripts/ci-jobs.sh | scripts/ci-jobs-test.sh)
    echo "$every_job"
    ;;
  scripts/chaos/* | scripts/chaos.sh) echo "go chaos" ;;
  scripts/docker-stops-evidence.sh) echo "chaos" ;;
  *_test.go) echo "$gotest" ;;
  # The fake Docker Engine is the tests', which reach no Swarm.
  internal/docker/dockertest/*) echo "$go" ;;
  internal/agent/panels/*.go | internal/control/importapi/*.go | internal/control/composeapi/*.go | internal/docker/*.go | \
    internal/resource/container/*.go | internal/resource/panel.go | internal/resource/resourcepb/*.go | internal/resource/host/*.go | \
    internal/revision/units.go | scripts/swarm-e2e.sh)
    echo "$swarm"
    ;;
  # go vet ./... compiles a Go file wherever it is, under docs/ too.
  *.go) echo "$go" ;;
  # The release archives carry both, and the snapshot builds them.
  LICENSE | README.md) echo "snapshot" ;;
  # Read by people. Licenses and links checks them, and it always runs.
  docs/* | .github/*.md | .github/ISSUE_TEMPLATE/* | .github/dependabot.yml | assets/*) ;;
  # Dockwell/catalog's snapshot: its entries, their images, and the commit it is.
  internal/catalog/snapshot/entries/* | internal/catalog/snapshot/commit) echo "$catalog" ;;
  # The Go tests source it, the load tests and the budgets read its memory
  # limit, and Service end to end on Docker uninstalls with it.
  scripts/install.sh) echo "go containers-load transfers-load docker-e2e $install $budgets" ;;
  # A Go test runs it, and so do the install legs, under mawk, gawk and busybox.
  scripts/connected.awk) echo "$go $install" ;;
  scripts/install-test/* | scripts/sigstore_trusted_root.json) echo "$install" ;;
  scripts/budgets.sh) echo "$budgets" ;;
  # Run by Licenses and links, by their own nightly workflows, or by a release.
  scripts/check-spdx.sh | scripts/check-trust-root.sh | scripts/check-cloudflare-ranges.sh | scripts/latest-release.sh | scripts/require-green.sh | scripts/adrs.sh) ;;
  .goreleaser.yaml) echo "$install $budgets" ;;
  # The toolchain, the modules and the tools, GoReleaser among them, build the release.
  go.mod | go.sum | tools/*) echo "$go $install" ;;
  cmd/* | internal/* | gen/* | proto/* | buf.yaml | buf.gen.yaml | buf.lock | .golangci.yml | scripts/*) echo "$go" ;;
  # The panel's stylesheet bundles the fonts.
  design/system/fonts/*) echo "$web" ;;
  packages/mockview/* | design/*) echo "$design" ;;
  web/* | packages/* | package.json | pnpm-lock.yaml | pnpm-workspace.yaml | biome.json | tsconfig.base.json | .nvmrc)
    echo "$web"
    ;;
  */*) echo "$every_job" ;;
  # CONTRIBUTING.md and the rest at the top.
  *.md) ;;
  *) echo "$every_job" ;;
  esac
}

# emit writes key=value where the workflow reads it, or to stdout.
emit() {
  if [[ -n ${GITHUB_OUTPUT:-} ]]; then
    printf '%s\n' "$1" >>"$GITHUB_OUTPUT"
  else
    printf '%s\n' "$1"
  fi
}

# reasons reads NUL-separated paths, or takes --all and why, and prints each
# job they need, in the order of every_job, with a tab and why it's needed.
reasons() {
  declare -A why=()
  local path job
  if [[ ${1:-} == --all ]]; then
    for job in $every_job; do why[$job]=${2:-"every job runs"}; done
  else
    # The last path counts even without a NUL after it.
    while IFS= read -r -d '' path || [[ -n $path ]]; do
      for job in $(jobs_for "$path") $(biome_checks "$path" && echo panel); do
        [[ -n ${why[$job]:-} ]] || why[$job]="\`$path\` needs it"
      done
    done
  fi
  for job in $every_job; do
    if [[ -n ${why[$job]:-} ]]; then printf '%s\t%s\n' "$job" "${why[$job]}"; fi
  done
}

# changed prints reasons for this run's change: the paths in $CI_JOBS_FILES
# when it's set, for the tests; for a pull request, where HEAD's tree differs
# from main's, which it fetches; or every job.
changed() {
  if [[ -n ${CI_JOBS_FILES:-} ]]; then
    reasons <"$CI_JOBS_FILES"
    return
  fi
  if [[ ${GITHUB_EVENT_NAME:-} != pull_request ]]; then
    reasons --all "this isn't a pull request, so every job runs"
    return
  fi
  local main=${CI_JOBS_MAIN:-main} depth=() paths
  # A shallow checkout fetches main shallow too. A full one stays full, as
  # --depth would cut the history API schema compares the schema with.
  if [[ $(git rev-parse --is-shallow-repository 2>/dev/null || true) == true ]]; then depth=(--depth=1); fi
  if ! git fetch -q --no-tags "${depth[@]}" origin "+refs/heads/$main:refs/ci-jobs/main" 2>/dev/null; then
    reasons --all "$main couldn't be fetched to compare with, so every job runs"
    return
  fi
  paths=$(mktemp)
  git diff -z --name-only --no-renames refs/ci-jobs/main HEAD >"$paths"
  reasons <"$paths"
  rm -f "$paths"
}

pick() {
  local json
  json=$(reasons "$@" | cut -f1 | jq -R . | jq -cs .)
  emit "jobs=$json"
}

needs() {
  (($# > 0)) || { echo "usage: scripts/ci-jobs.sh needs <job>..." >&2; return 2; }
  local all job why first=1 lines=() every
  all=$(changed)
  for job in "$@"; do
    [[ -n ${names[$job]:-} ]] || {
      echo "ci-jobs: no rule names a job $job. Add it to scripts/ci-jobs.sh, with examples in scripts/ci-jobs-test.sh." >&2
      return 2
    }
    why=$(awk -F'\t' -v j="$job" '$1 == j { print $2 }' <<<"$all")
    if [[ -n $why ]]; then
      emit "$job=true"
      ((first)) && emit "run=true"
      lines+=("This change needs ${names[$job]}: $why.")
    else
      emit "$job=false"
      ((first)) && emit "run=false"
      lines+=("This change doesn't need ${names[$job]}: none of the files it changes do.")
    fi
    first=0
  done
  [[ -n $(awk -F'\t' -v j="$1" '$1 == j' <<<"$all") ]] || lines+=("So this job's other steps are skipped.")
  emit "jobs=$(cut -f1 <<<"$all" | jq -R 'select(. != "")' | jq -cs .)"

  if [[ " $* " == *" install "* || " $* " == *" systemd "* ]]; then
    every=$(awk -F'\t' '$1 == "every-leg" { print $2 }' <<<"$all")
    if [[ -n $every ]]; then
      emit "install-matrix=$(jq -cn --arg images "$install_images" --arg arches "$arches" \
        '{include: [($images | split(" ")[]) as $i | ($arches | split(" ")[]) as $a | {image: $i, arch: $a}]}')"
      emit "systemd-matrix=$(jq -cn --arg arches "$arches" '{arch: ($arches | split(" "))}')"
      lines+=("The install legs run on every image and architecture: $every.")
    else
      emit "install-matrix=$(jq -c '{include: .}' <<<"$some_install_legs")"
      emit "systemd-matrix=$(jq -c '{arch: .}' <<<"$some_systemd_arches")"
      lines+=("If they run, the install legs run on $(jq -r 'map("\(.image) (\(.arch))") | join(" and ")' <<<"$some_install_legs"), and systemd on $(jq -r 'join(" and ")' <<<"$some_systemd_arches"). A change to the install script, its tests or the release runs them all.")
    fi
  fi

  printf '%s\n' "${lines[@]}"
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
    {
      printf '%s\n' "${lines[@]}"
      echo
      echo "\`scripts/ci-jobs.sh\` says which files need which jobs."
    } >>"$GITHUB_STEP_SUMMARY"
  fi
}

check() {
  : "${NEEDS:?set NEEDS to the needs context, as JSON}"
  local picked bad
  # Without what Release snapshot picked, every job is held to passing.
  picked=${JOBS:-}
  [[ -n $picked ]] || picked=$(reasons --all | cut -f1 | jq -R . | jq -cs .)
  bad=$(jq -rn --argjson needs "$NEEDS" --argjson picked "$picked" '
    $needs | to_entries[] | .key as $job | .value.result as $result
    | select($job != "base")
    | if ($picked | index($job)) then
        select($result != "success") | "\($job): \($result), and this change needs it"
      else
        select($result != "success" and $result != "skipped") | "\($job): \($result)"
      end')
  if [[ -n $bad ]]; then
    local lines
    mapfile -t lines <<<"$bad"
    echo "These jobs didn't pass:"
    printf '  %s\n' "${lines[@]}"
    echo "Open each one's log to see why, then push a fix or re-run it."
    return 1
  fi
  echo "Every job this change needs passed, and the rest weren't needed: $picked"
}

case ${1:-} in
pick) shift && pick "$@" ;;
needs) shift && needs "$@" ;;
check) check ;;
*)
  echo "usage: scripts/ci-jobs.sh needs <job>..., scripts/ci-jobs.sh check, or scripts/ci-jobs.sh pick [--all] < paths" >&2
  exit 2
  ;;
esac
