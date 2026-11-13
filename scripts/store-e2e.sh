#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Tries apps from other stores on this machine's Docker Engine, through the CLI, as a
# person would (ADR 0085). Dockwell reads 1Panel's app store, Dokploy's templates and
# Coolify's at the commits internal/catalog/convert/rates.json pins, converting each
# on the server as it reads it, and each app named installs from its converted entry
# as a project of its own: its plan pins each image by digest, every container runs,
# healthy where its entry has a health check, on loopback alone, and the app answers
# on the port a person opens. Then it goes again through a plan. So "converts" means
# "works" for a sample of what each store brings, as make store-rates counts the rest.
#
# It uses throwaway data directories and removes what it made, so it never touches a
# real install. It needs Docker, Go, curl and jq, and reaches GitHub for the stores and
# their registries for the images. With E2E_ROOT set, as in CI, Dockwell runs as root
# through sudo, as it does on a server.
#
#   scripts/store-e2e.sh bin/dockwell 1panel/linux-command dokploy/excalidraw
set -euo pipefail
# Anything this starts asks nothing beyond this machine but what it's asked to read
# (DOCKWELL_TEST, ADR 0034): the stores, and the registries of the images their apps
# run, which Docker pulls from anyway, for their digests.
export DOCKWELL_TEST=1 DOCKWELL_TEST_REGISTRY=public

usage="usage: scripts/store-e2e.sh path/to/dockwell FORMAT/APP..."
for tool in docker curl go jq; do
  command -v "$tool" >/dev/null || {
    echo "store-e2e: $tool isn't installed, and the script needs it. Install it, such as with apt-get install $tool, then run it again" >&2
    exit 1
  }
done
bin=$(realpath "${1:?$usage}")
shift
(($# > 0)) || {
  echo "$usage" >&2
  exit 1
}
root=$(cd "$(dirname "$0")/.." && pwd)
rates=$root/internal/catalog/convert/rates.json
addr=127.0.0.1:${E2E_PORT:-7699}
port=${addr##*:}
data=$(mktemp -d)
log=$(mktemp)
cookie=$(mktemp)
passkey=$(mktemp)
projects=()
pid=""

as_dockwell=()
[[ -z "${E2E_ROOT:-}" ]] || as_dockwell=(sudo -n)
priv() { "${as_dockwell[@]}" "$@"; }

cleanup() {
  for p in "${projects[@]}"; do
    docker ps --all --quiet --filter "label=com.docker.compose.project=$p" | xargs --no-run-if-empty docker rm --force --volumes >/dev/null 2>&1 || true
    docker volume ls --quiet --filter "label=com.docker.compose.project=$p" | xargs --no-run-if-empty docker volume rm --force >/dev/null 2>&1 || true
    docker network ls --quiet --filter "label=com.docker.compose.project=$p" | xargs --no-run-if-empty docker network rm >/dev/null 2>&1 || true
  done
  [[ -z "$pid" ]] || priv kill "$pid" 2>/dev/null || true
  wait 2>/dev/null || true
  priv rm -rf "$data" "$log" "$cookie" "$passkey"
}
trap cleanup EXIT

# project is the Compose project of the app being tried, which its containers carry.
project=""
fail() {
  echo "store-e2e: $*" >&2
  if [[ -n "$project" ]]; then
    echo "--- what runs for it" >&2
    docker ps --all --filter "label=com.docker.compose.project=$project" --format '{{.Names}} {{.Image}} {{.Status}}' >&2 || true
    for c in $(docker ps --all --quiet --filter "label=com.docker.compose.project=$project" 2>/dev/null); do
      echo "--- the log of $(docker inspect --format '{{.Name}}' "$c" 2>/dev/null)" >&2
      docker logs --tail 40 "$c" >&2 2>&1 || true
    done
  fi
  echo "--- dockwell log, its last 200 lines" >&2
  priv tail -n 200 "$log" >&2 || true
  exit 1
}
step() { echo "--- $*"; }

dw() { priv env DOCKWELL_DATA_DIR="$data" DOCKWELL_LOG_FORMAT=text "$bin" "$@"; }

step "Dockwell starts, and its owner claims it"
"${as_dockwell[@]}" env DOCKWELL_TEST=1 DOCKWELL_TEST_REGISTRY=public \
  "$bin" run --listen "$addr" --agent-listen 127.0.0.1:0 --data-dir "$data" --log-format json >>"$log" 2>&1 &
pid=$!
for _ in $(seq 100); do
  curl -fsS "http://$addr/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
[[ "$(curl -fsS "http://$addr/healthz")" == "ok" ]] || fail "/healthz did not answer ok within 10 s"
for _ in $(seq 300); do
  priv grep -q 'connected to the control plane' "$log" && break
  sleep 0.1
done
priv grep -q 'connected to the control plane' "$log" || fail "the control server's own agent didn't connect within 30 s"
claim=$(dw claim) || fail "dockwell claim failed"
link=$(grep -o 'http://localhost:[0-9]*/claim#[A-Za-z0-9_-]*' <<<"$claim" | head -1) || true
[[ -n "$link" ]] || fail "dockwell claim printed no claim link: $claim"
(cd "$root" && go run ./scripts/claim "$link" "$cookie" "$passkey") || fail "can't claim the install"
dw login --local >/dev/null || fail "dockwell login --local failed"

# person_answer calls the API as a signed-in person does, from the panel's origin, and
# says what it answered, its headers first, and the status last.
person_answer() {
  curl -sS --max-time 30 -D - -w '\n%{http_code}' --resolve "localhost:$port:127.0.0.1" -H "Origin: http://localhost:$port" -H 'Content-Type: application/json' \
    -H "Cookie: $(cat "$cookie")" -d "$2" "http://localhost:$port/dockwell.v1.$1"
}
change_field() { dw changes get "$1" --json | jq -r ".change.$2"; }
# settles waits for a change to finish applying: 0 once it's applied, 1 once it failed.
settles() {
  local state=""
  for _ in $(seq 1200); do
    state=$(change_field "$1" state) || state=""
    [[ "$state" == CHANGE_STATE_APPLIED ]] && return 0
    [[ "$state" == CHANGE_STATE_FAILED ]] && return 1
    sleep 0.5
  done
  fail "$1 is $state after 10 min, want applied"
}
# try_apply applies a planned change: with the CLI's token when the plan lets one, and
# as a person does in the panel when it destroys data, as removing an app does. A store's
# sample never reaches the server: a plan with host access fails it, as no store's app
# should need any, and a person would have to approve it. It returns 1 when the change
# failed, and fails at once on anything else.
try_apply() {
  local plan fingerprint answer
  plan=$(dw changes get "$1" --json) || fail "can't read $1"
  if jq -e '.change.plan.hostAccess // [] | length > 0' <<<"$plan" >/dev/null; then
    fail "$1 reaches the server, which no store's sample may: $(jq -c '.change.plan.hostAccess' <<<"$plan")"
  fi
  if jq -e '.change.plan.destructive | not' <<<"$plan" >/dev/null; then
    dw changes apply "$1" --yes >/dev/null && return 0
    [[ "$(change_field "$1" state)" == CHANGE_STATE_FAILED ]] && return 1
    fail "applying $1 failed: $(dw changes get "$1" --json)"
  fi
  fingerprint=$(jq -r .change.plan.fingerprint <<<"$plan")
  answer=$(person_answer ChangeService/ApplyChange "{\"id\":\"$1\",\"fingerprint\":\"$fingerprint\"}") || fail "a person can't reach the API to apply $1"
  [[ "${answer##*$'\n'}" == 200 ]] || fail "a person can't apply $1: $answer"
  settles "$1"
}
apply() { try_apply "$1" || fail "$1 failed: $(dw changes get "$1" --json)"; }
# registry_trouble says a failed change failed on a registry, and nothing about the
# image or the entry, as catalog-e2e.sh reads it.
registry_trouble() {
  dw changes get "$1" --json | jq -e '.change.steps // [] | any(.reason == "registry" or ((.message // "") | contains("can'"'"'t reach the registry")))' >/dev/null
}

# A store's source, by the format its record names: a name a source may take.
declare -A source_of=([1panel]=onepanel [dokploy]=dokploy [coolify]=coolify)

step "Dockwell reads each store at the commit the repository pins, converting it as it reads it"
declare -A added=()
for app in "$@"; do
  format=${app%%/*}
  [[ -n "${source_of[$format]:-}" && "$app" == */* ]] || fail "name each app as FORMAT/APP, with FORMAT one of ${!source_of[*]}, not $app"
  [[ -n "${added[$format]:-}" ]] && continue
  read -r repository commit < <(jq -r --arg f "$format" '.stores[] | select(.format == $f) | "\(.repository) \(.commit)"' "$rates")
  [[ -n "${commit:-}" ]] || fail "$rates pins no commit of $format"
  planned=$(dw catalog set-source --name "${source_of[$format]}" --repository "$repository" --commit "$commit" --plan-only --json) ||
    fail "planning to read $repository at $commit failed"
  change=$(jq -r '.change.id // empty' <<<"$planned")
  [[ "$change" == chg_* ]] || fail "reading $repository planned no change: $planned"
  jq -r '.change.plan.notes[]? | select(.reason == "catalog_source_reads" or .reason == "catalog_source_leaves_out") | "  " + .message' <<<"$planned"
  apply "$change"
  added[$format]=1
done

for app in "$@"; do
  started=$SECONDS
  format=${app%%/*}
  id=${app#*/}
  source=${source_of[$format]}
  step "$app installs from its store, runs and answers"
  entry=$(dw catalog get "$id" --catalog "$source" --json | jq .entry) || fail "the catalog has no $id from $source"
  [[ "$entry" != null ]] || fail "the catalog has no $id from $source"
  planned=$(dw catalog install "$id" --catalog "$source" --plan-only --json) || fail "planning to install $id from $source failed"
  change=$(jq -r '.change.id // empty' <<<"$planned")
  [[ "$change" == chg_* ]] || fail "installing $id planned no change: $planned"
  jq -e '.change.plan.notes | any(.reason == "catalog_pins_images")' <<<"$planned" >/dev/null || fail "installing $id doesn't say the builds it pins: $planned"
  installed=$(jq -r '[.change.plan.resources[] | select(.resource.kind == "project")][0].resource.name // empty' <<<"$planned")
  environment=$(jq -r '[.change.plan.resources[] | select(.resource.kind == "service")][0].resource.scope' <<<"$planned")
  project=${environment,,}
  projects+=("$project")
  if ! try_apply "$change"; then
    registry_trouble "$change" || fail "$change failed: $(dw changes get "$change" --json)"
    echo "store-e2e: $change failed on a registry, not on the entry. Planning it again in 30 s, once" >&2
    sleep 30
    planned=$(dw catalog install "$id" --catalog "$source" --project "$installed-2" --plan-only --json) || fail "planning to install $id again failed"
    environment=$(jq -r '[.change.plan.resources[] | select(.resource.kind == "service")][0].resource.scope' <<<"$planned")
    installed="$installed-2"
    project=${environment,,}
    projects+=("$project")
    apply "$(jq -r .change.id <<<"$planned")"
  fi

  # Every container runs an image pinned by digest, on loopback alone, and turns
  # healthy where its entry has a check; one with none keeps running.
  names=$(docker ps --all --filter "label=com.docker.compose.project=$project" --format '{{.Names}}') || fail "docker ps failed"
  [[ -n "$names" ]] || fail "$id runs nothing once its install has applied"
  for c in $names; do
    image=$(docker inspect --format '{{.Config.Image}}' "$c") || fail "docker inspect $c failed"
    [[ "$image" == *@sha256:* ]] || fail "$c runs $image, not pinned by digest"
    bound=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$c") || fail "docker inspect $c failed"
    jq -e '[(. // {})[][] | .HostIp] | all(. == "127.0.0.1")' <<<"$bound" >/dev/null || fail "$c is published past this server: $bound"
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$c") || fail "docker inspect $c failed"
    if [[ "$health" == none ]]; then
      sleep 10
      [[ "$(docker inspect --format '{{.State.Running}} {{.RestartCount}}' "$c")" == "true 0" ]] || fail "$c stopped or restarted within 10 s of its install"
    else
      [[ "$health" == healthy ]] || fail "$c reads $health once its install has applied, want healthy"
    fi
  done
  web=$(jq -r '.web.service // empty' <<<"$entry")
  if [[ -n "$web" ]]; then
    c=$(docker ps --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$web" --format '{{.Names}}' | head -1)
    hostport=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$c" | jq -r '[(. // {})[][] | .HostPort][0] // empty')
    [[ -n "$hostport" ]] || fail "$web publishes no port"
    answered=""
    for _ in $(seq 60); do
      answered=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$hostport/" 2>/dev/null) || answered=""
      [[ -n "$answered" && "$answered" != 000 && "$answered" -lt 500 ]] && break
      sleep 1
    done
    [[ -n "$answered" && "$answered" != 000 && "$answered" -lt 500 ]] || fail "$id answers '$answered' on port $hostport a minute after its install, want a page"
    echo "  $id answers $answered on port $hostport"
  fi

  step "$app goes through a plan"
  all=$(dw resources list --json) || fail "listing resources failed"
  edits=$(jq -c --arg env "$environment" --arg p "$installed" '[.resources[] | select(.resource.scope == $env) | {resource: .resource, delete: true}] + [{resource: {kind: "project", name: $p}, delete: true}] | .[]' <<<"$all")
  flags=()
  while read -r e; do flags+=(--edits "$e"); done <<<"$edits"
  planned=$(dw changes propose --summary "Remove $id" "${flags[@]}" --plan-only --json) || fail "planning to remove $id failed"
  apply "$(jq -r .change.id <<<"$planned")"
  left=$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project") || fail "docker ps failed"
  [[ -z "$left" ]] || fail "removing $id left containers"
  echo "$app took $((SECONDS - started)) s"
  project=""
done

echo "store-e2e: ok"
