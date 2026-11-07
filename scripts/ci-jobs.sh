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
# A job's ID is its key in the workflow. A pull request's files are what its
# merge commit changes against main. A push to main, a nightly run and
# anything but a pull request need every job. So does a change to a workflow,
# the Makefile or this script, or a path no rule here knows, so a new kind of
# file is never left untested.
#
# needs writes run=true or false for the first job, and <job>=true or false
# for each, to $GITHUB_OUTPUT, and says why in $GITHUB_STEP_SUMMARY. Each CI
# job runs it first and skips its other steps when it isn't needed, rather
# than waiting for a job that picks: a job that waits for another goes to the
# back of the queue when that one ends, and the queue is where a push spends
# most of its time.
#
# check is Install, every job, the check a branch rule requires of the Install
# workflow, whose install legs are a matrix. It fails unless every job this
# change needs passed, and every other job passed or was skipped: a branch rule
# counts a skipped job as passing. Build of main may fail, as it only adds a
# comparison. NEEDS is the needs context, as JSON.
#
# scripts/ci-jobs-test.sh holds all three to examples, in Licenses and links.
set -euo pipefail
shopt -s inherit_errexit

# Every job either workflow gates on what changed, in the order they're
# listed. Licenses and links and Install, every job always run, and Build of
# main runs on every pull request, as it can't run this before it builds.
ci_jobs="go docker-go caddy-go docker-e2e catalog-docker docker-panel logs-load containers-load e2e schema panel"
install_jobs="snapshot budgets install systemd"
every_job="$ci_jobs $install_jobs"

# What each job is called in its workflow.
declare -A names=(
  [go]="Go" [docker-go]="Go on Docker" [caddy-go]="Go on Caddy" [docker-e2e]="Service end to end on Docker"
  [catalog-docker]="Catalog on Docker" [docker-panel]="Panel end to end on Docker" [logs-load]="Log load"
  [containers-load]="Hundreds of containers" [e2e]="Panel end to end" [schema]="API schema" [panel]="Panel"
  [snapshot]="Release snapshot" [budgets]="Memory budgets"
  [install]="Install on each image" [systemd]="Install with systemd"
)

# Go code, or what the Go tests, the load tests and the end-to-end jobs read or
# run. The idle memory budgets move with any of it.
go="$ci_jobs snapshot budgets"
# The panel and its packages: their lint and tests, the generated code, and the
# end-to-end jobs that drive the panel. Only the full build embeds the panel,
# and it reads each file once at start, so the budgets don't run for it.
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
# The install script, the release archives it installs, and what it runs in
# the binary: version, role, claim, join, token and run, from internal/cli,
# with the build's name and version and where its data goes by default.
install="snapshot install systemd"
budgets="snapshot budgets"

# jobs_for prints the jobs that a change to the path needs. The first pattern
# that matches wins, and * matches across slashes.
jobs_for() {
  case $1 in
  .github/workflows/* | .github/actions/* | Makefile | scripts/ci-jobs.sh | scripts/ci-jobs-test.sh)
    echo "$every_job"
    ;;
  # Read by people. Licenses and links checks them, and it always runs.
  docs/* | .github/*.md | .github/ISSUE_TEMPLATE/* | .github/dependabot.yml | assets/* | LICENSE) ;;
  internal/catalog/entries/*.yaml) echo "$catalog" ;;
  # The Go tests source it, and the load tests and the budgets read its memory limit.
  scripts/install.sh) echo "go containers-load $install $budgets" ;;
  scripts/install-test/* | scripts/sigstore_trusted_root.json) echo "$install" ;;
  scripts/budgets.sh) echo "$budgets" ;;
  # Run by Licenses and links, by their own nightly workflows, or by a release.
  scripts/check-spdx.sh | scripts/check-trust-root.sh | scripts/check-cloudflare-ranges.sh | scripts/latest-release.sh | scripts/require-green.sh) ;;
  .goreleaser.yaml) echo "$install $budgets" ;;
  *_test.go) echo "$gotest" ;;
  # The commands the install script doesn't run.
  internal/cli/*/* | internal/cli/api.go | internal/cli/openbackup.go | internal/cli/rotatekey.go) echo "$go" ;;
  internal/cli/* | internal/buildinfo/* | internal/datadir/* | internal/ticket/* | cmd/dockwell/* | go.mod | go.sum | tools/*)
    echo "$go $install"
    ;;
  cmd/* | internal/* | gen/* | proto/* | buf.yaml | buf.gen.yaml | buf.lock | .golangci.yml | scripts/*) echo "$go" ;;
  packages/mockview/* | design/*) echo "$design" ;;
  web/* | packages/* | package.json | pnpm-lock.yaml | pnpm-workspace.yaml | biome.json | tsconfig.base.json | .nvmrc)
    echo "$web"
    ;;
  */*) echo "$every_job" ;;
  # README.md, CONTRIBUTING.md and the rest at the top.
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

# reasons reads NUL-separated paths, or takes --all, and prints each job they
# need, in the order of every_job, with a tab and the first path that needs it.
reasons() {
  declare -A why=()
  local path job
  if [[ ${1:-} == --all ]]; then
    for job in $every_job; do why[$job]="every job, as this isn't a pull request"; done
  else
    # The last path counts even without a NUL after it.
    while IFS= read -r -d '' path || [[ -n $path ]]; do
      for job in $(jobs_for "$path"); do
        [[ -n ${why[$job]:-} ]] || why[$job]="\`$path\` needs it"
      done
    done
  fi
  for job in $every_job; do
    if [[ -n ${why[$job]:-} ]]; then printf '%s\t%s\n' "$job" "${why[$job]}"; fi
  done
}

# changed prints reasons for this run's change: the paths in $CI_JOBS_FILES
# when it's set, for the tests; a pull request's, from its merge commit, which
# the checkout needs with fetch-depth: 2; or every job.
changed() {
  if [[ -n ${CI_JOBS_FILES:-} ]]; then
    reasons <"$CI_JOBS_FILES"
  elif [[ ${GITHUB_EVENT_NAME:-} == pull_request ]] && git rev-parse -q --verify HEAD^2 >/dev/null; then
    local paths
    paths=$(mktemp)
    git diff -z --name-only --no-renames HEAD^1 HEAD >"$paths"
    reasons <"$paths"
    rm -f "$paths"
  else
    reasons --all
  fi
}

pick() {
  local json
  json=$(reasons "$@" | cut -f1 | jq -R . | jq -cs .)
  emit "jobs=$json"
}

needs() {
  (($# > 0)) || { echo "usage: scripts/ci-jobs.sh needs <job>..." >&2; return 2; }
  local all job why first=1 lines=()
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
  picked=$(changed | cut -f1 | jq -R . | jq -cs .)
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
