# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# openlist's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_openlist first, once it's installed,
# and check_openlist upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[openlist]="4.2.7 openlistteam/openlist:v4.2.7@sha256:11b490123d57f88c84ecda46285e1921869cf511da3e5bb4062f183e7e1200b7"

check_openlist() {
  local port base said token password
  port=$(value PORT)
  base="http://127.0.0.1:$port"
  [[ "$(curl -fsS --max-time 10 "$base/ping")" == pong ]] || fail "OpenList's ping doesn't answer"
  if [[ "$1" == first ]]; then
    kept[openlist-password]="$(openssl rand -hex 24)"
    said=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg p "${kept[openlist-password]}" '{username:"owner",password:$p,site_title:"Kept through upgrade"}')" "$base/api/public/init/setup") || fail "OpenList's setup doesn't answer"
    jq -e '.code == 200' <<<"$said" >/dev/null || fail "OpenList couldn't make its owner"
  fi
  password=${kept[openlist-password]}
  said=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' -d "$(jq -nc --arg p "$password" '{username:"owner",password:$p}')" "$base/api/auth/login") || fail "OpenList's sign-in doesn't answer"
  token=$(jq -er '.data.token | select(length > 0)' <<<"$said") || fail "OpenList doesn't sign in as its owner"
  said=$(curl -fsS --max-time 15 -H "Authorization: $token" "$base/api/admin/setting/get?key=site_title") || fail "OpenList can't read its settings"
  jq -e '.code == 200 and .data.value == "Kept through upgrade"' <<<"$said" >/dev/null || fail "OpenList lost its title through upgrade"
  said=$(curl -sS --max-time 15 -H 'Content-Type: application/json' -d '{"username":"owner","password":"not-the-password"}' "$base/api/auth/login") || fail "OpenList's sign-in doesn't answer"
  jq -e '.code == 401 and (.data.token // "") == ""' <<<"$said" >/dev/null || fail "OpenList takes a wrong password"
  said=$(curl -fsS --max-time 10 "$base/api/public/init_status") || fail "OpenList's setup state doesn't answer"
  jq -e '.code == 200 and .data.initialized' <<<"$said" >/dev/null || fail "OpenList forgot that setup was finished"
  said=$(docker logs "$(container openlist)" 2>&1) || fail "OpenList's log can't be read"
  [[ "$said" != *"$password"* ]] || fail "OpenList writes its setup password in its log"
}
