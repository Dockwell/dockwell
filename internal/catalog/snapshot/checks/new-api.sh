# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# new-api's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_new-api first, once it's installed,
# and check_new-api upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[new-api]="0.13.1-patch.1 calciumion/new-api:v0.13.1-patch.1@sha256:95fab02db3925f8028f8f43b333734893a3c568ac555fa3b5df74650286d9ce3"

check_new-api() {
  local port base password jar said user
  port=$(value PORT)
  base="http://127.0.0.1:$port"
  generated new-api SESSION_SECRET >/dev/null
  if [[ "$1" == first ]]; then
    kept[new-api-password]="$(openssl rand -hex 24)"
    said=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg p "${kept[new-api-password]}" '{username:"owner",password:$p,confirmPassword:$p,SelfUseModeEnabled:true,DemoSiteEnabled:false}')" "$base/api/setup") || fail "new-api's setup doesn't answer"
    jq -e '.success' <<<"$said" >/dev/null || fail "new-api couldn't make its owner"
  fi
  password=${kept[new-api-password]}
  jar=$(mktemp)
  said=$(curl -fsS --max-time 15 -c "$jar" -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg p "$password" '{username:"owner",password:$p}')" "$base/api/user/login") || fail "new-api's sign-in doesn't answer"
  jq -e '.success and .data.username == "owner"' <<<"$said" >/dev/null || fail "new-api doesn't sign in as its owner"
  user=$(jq -er .data.id <<<"$said") || fail "new-api's owner has no ID"
  if [[ "$1" == first ]]; then
    said=$(curl -fsS --max-time 15 -b "$jar" -H "New-Api-User: $user" -H 'Content-Type: application/json' \
      -d '{"name":"kept-through-upgrade","expired_time":-1,"unlimited_quota":true}' "$base/api/token/") || fail "new-api can't make an API token"
    jq -e '.success' <<<"$said" >/dev/null || fail "new-api didn't make an API token"
  fi
  said=$(curl -fsS --max-time 15 -b "$jar" -H "New-Api-User: $user" "$base/api/token/") || fail "new-api can't read its tokens"
  jq -e '.success and (.data.items | any(.name == "kept-through-upgrade"))' <<<"$said" >/dev/null || fail "new-api lost its API token through upgrade"
  said=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' -d '{"username":"owner","password":"not-the-password"}' "$base/api/user/login") || fail "new-api's sign-in doesn't answer"
  jq -e '.success == false' <<<"$said" >/dev/null || fail "new-api takes a wrong password"
  said=$(curl -fsS --max-time 10 "$base/api/setup") || fail "new-api's setup state doesn't answer"
  jq -e '.success and .data.status' <<<"$said" >/dev/null || fail "new-api forgot that setup was finished"
  said=$(docker logs "$(container new-api)" 2>&1) || fail "new-api's log can't be read"
  [[ "$said" != *"$password"* ]] || fail "new-api writes its setup password in its log"
  rm -f "$jar"
}
