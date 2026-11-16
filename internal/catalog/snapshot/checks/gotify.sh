# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# gotify's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_gotify first, once it's installed,
# and check_gotify upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[gotify]="3.1.0 gotify/server:3.1.0@sha256:be44495e4609c6f5b06c01a9445ec6198dba902ce49fc3d0d71ab585cda8014c"

check_gotify() {
  local port base password said app token
  port=$(value PORT)
  base="http://127.0.0.1:$port"
  password=$(generated gotify GOTIFY_DEFAULTUSER_PASS)
  said=$(curl -fsS --max-time 10 "$base/health") || fail "Gotify's database health doesn't answer"
  jq -e '.health == "green" and .database == "green"' <<<"$said" >/dev/null || fail "Gotify's database isn't healthy"
  said=$(curl -sS --max-time 10 -u admin:not-the-password -o /dev/null -w '%{http_code}' "$base/current/user") || fail "Gotify's sign-in doesn't answer"
  [[ "$said" == 401 ]] || fail "Gotify answers $said to a wrong password, want 401"
  said=$(curl -fsS --max-time 10 -u "admin:$password" "$base/current/user") || fail "Gotify doesn't take its generated password"
  jq -e '.name == "admin" and .admin' <<<"$said" >/dev/null || fail "Gotify doesn't sign in as the admin"
  if [[ "$1" == first ]]; then
    said=$(curl -fsS --max-time 10 -u "admin:$password" -H 'Content-Type: application/json' -d '{"name":"Catalog e2e"}' "$base/application") || fail "Gotify doesn't create an application"
    app=$(jq -er '.id' <<<"$said") || fail "Gotify created no application ID"
    token=$(jq -er '.token | select(length > 0)' <<<"$said") || fail "Gotify created no application token"
    kept[gotify-app]=$app
    kept[gotify-token]=$token
    curl -fsS --max-time 10 -H "X-Gotify-Key: $token" -H 'Content-Type: application/json' \
      -d '{"title":"Catalog upgrade","message":"kept-through-upgrade","priority":1}' "$base/message" >/dev/null || fail "Gotify doesn't take a notification"
  else
    said=$(curl -fsS --max-time 10 -u "admin:$password" "$base/application") || fail "Gotify can't read its applications after upgrade"
    # The list omits private tokens. Using the original token proves it stayed;
    # a different message keeps that proof apart from the one already stored.
    jq -e --argjson id "${kept[gotify-app]}" 'any(.id == $id and .name == "Catalog e2e")' <<<"$said" >/dev/null || fail "Gotify lost its application through upgrade"
    said=$(curl -fsS --max-time 10 -H "X-Gotify-Key: ${kept[gotify-token]}" -H 'Content-Type: application/json' \
      -d '{"title":"Catalog upgrade","message":"sent-after-upgrade","priority":1}' "$base/message") || fail "Gotify no longer accepts its original application token"
    jq -e --argjson id "${kept[gotify-app]}" '.appid == $id and .message == "sent-after-upgrade"' <<<"$said" >/dev/null || fail "Gotify didn't send through the original application"
  fi
  said=$(curl -fsS --max-time 10 -u "admin:$password" "$base/message") || fail "Gotify can't read its notifications"
  jq -e --argjson id "${kept[gotify-app]}" '.messages | any(.appid == $id and .message == "kept-through-upgrade")' <<<"$said" >/dev/null || fail "Gotify lost its notification through upgrade"
  if [[ "$1" == upgraded ]]; then
    jq -e --argjson id "${kept[gotify-app]}" '.messages | any(.appid == $id and .message == "sent-after-upgrade")' <<<"$said" >/dev/null || fail "Gotify didn't keep its new notification"
  fi
}
