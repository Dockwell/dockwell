# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# uptime-kuma's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_uptime-kuma first, once it's installed,
# and check_uptime-kuma upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[uptime-kuma]="2.5.7 louislam/uptime-kuma:2.5.7@sha256:4b95dbe7ac5343b768f8e04e3035ccd59997872b1a35ab9b0f082d2cc279070e"

# kuma_emit sends Uptime Kuma one event over Socket.IO's HTTP polling, as its own pages
# talk to it, and prints what it acknowledged. Its sign-in and setup have no other API.
kuma_emit() {
  local base="http://127.0.0.1:$1/socket.io/?EIO=4&transport=polling" said sid
  said=$(curl -fsS --max-time 10 "$base") || fail "Uptime Kuma's socket doesn't answer"
  sid=$(sed -n 's/^0{"sid":"\([^"]*\)".*/\1/p' <<<"$said")
  [[ -n "$sid" ]] || fail "Uptime Kuma's socket opened with '${said:0:200}'"
  curl -fsS --max-time 10 -H 'Content-Type: text/plain;charset=UTF-8' --data-binary 40 "$base&sid=$sid" >/dev/null || fail "Uptime Kuma's socket didn't connect"
  curl -fsS --max-time 10 "$base&sid=$sid" >/dev/null || fail "Uptime Kuma's socket didn't connect"
  curl -fsS --max-time 10 -H 'Content-Type: text/plain;charset=UTF-8' --data-binary "420$2" "$base&sid=$sid" >/dev/null || fail "Uptime Kuma didn't take $2"
  for _ in $(seq 10); do
    said=$(curl -fsS --max-time 30 "$base&sid=$sid") || fail "Uptime Kuma's socket closed"
    said=$(tr '\036' '\n' <<<"$said" | sed -n 's/^430//p')
    [[ -n "$said" ]] && break
  done
  [[ -n "$said" ]] || fail "Uptime Kuma didn't answer $2"
  echo "$said"
}

check_uptime-kuma() {
  local port said
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/entry-page") || fail "Uptime Kuma doesn't answer on port $port"
  jq -e '.type != "setup-database"' <<<"$said" >/dev/null || fail "Uptime Kuma asks which database to use, which its entry sets: $said"
  docker exec "$(container uptime-kuma)" test -s /app/data/kuma.db || fail "Uptime Kuma keeps no database on its volume"
  if [[ "$1" == first ]]; then
    said=$(kuma_emit "$port" '["setup","owner","E2e-owner-passw0rd!"]')
    jq -e '.[0].ok == true' <<<"$said" >/dev/null || fail "Uptime Kuma didn't make its admin: $said"
  fi
  # Its admin is in its database, so they sign in after an upgrade too.
  said=$(kuma_emit "$port" '["login",{"username":"owner","password":"E2e-owner-passw0rd!","token":""}]')
  jq -e '.[0].ok == true and (.[0].token | length > 0)' <<<"$said" >/dev/null || fail "Uptime Kuma's admin can't sign in: $said"
  said=$(kuma_emit "$port" '["login",{"username":"owner","password":"not-the-password","token":""}]')
  jq -e '.[0].ok == false' <<<"$said" >/dev/null || fail "Uptime Kuma didn't refuse a wrong password: $said"
}
