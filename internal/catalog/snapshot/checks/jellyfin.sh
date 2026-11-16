# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# jellyfin's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_jellyfin first, once it's installed,
# and check_jellyfin upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[jellyfin]="12.2 jellyfin/jellyfin:12.2@sha256:357724bf0ae27a672c7cbaa899db2d9abeb13dbd8657ccce750258a4c059d037"

# What a person gives its install, as dockwell catalog install takes it.
given[jellyfin]="MEDIA_DIR=$media_dir"

# jellyfin_sign_in signs in to Jellyfin as its apps do, and says the status.
jellyfin_sign_in() {
  curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    -H 'Authorization: MediaBrowser Client="e2e", Device="e2e", DeviceId="e2e-catalog", Version="1.0"' \
    -d "{\"Username\":\"owner\",\"Pw\":\"$2\"}" "http://127.0.0.1:$1/Users/AuthenticateByName"
}

check_jellyfin() {
  local jellyfin port said
  jellyfin=$(container jellyfin)
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/System/Info/Public") || fail "Jellyfin doesn't answer on port $port"
  [[ "$(jq -r .Version <<<"$said")" == "$version"* ]] || fail "Jellyfin says $said, want $version"
  docker exec "$jellyfin" test -r /media/e2e.txt || fail "Jellyfin can't read the media folder it was given"
  if docker exec "$jellyfin" touch /media/written 2>/dev/null; then fail "Jellyfin can write to the media folder, which it reads alone"; fi
  if [[ "$1" == first ]]; then
    # Its setup page, as the first person to open it goes through it.
    jf() { curl -fsS --max-time 15 -o /dev/null -H 'Content-Type: application/json' "$@"; }
    jf -d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' "http://127.0.0.1:$port/Startup/Configuration" || fail "Jellyfin's setup doesn't take its settings"
    curl -fsS --max-time 15 -o /dev/null "http://127.0.0.1:$port/Startup/User" || fail "Jellyfin's setup has no first user"
    jf -d '{"Name":"owner","Password":"E2e-owner-passw0rd"}' "http://127.0.0.1:$port/Startup/User" || fail "Jellyfin's setup doesn't take its first user"
    jf -X POST "http://127.0.0.1:$port/Startup/Complete" || fail "Jellyfin's setup doesn't finish"
  fi
  # Its account is on its volume, so it signs in after an upgrade too.
  said=$(jellyfin_sign_in "$port" E2e-owner-passw0rd) || fail "Jellyfin's sign-in got no answer"
  [[ "$said" == 200 ]] || fail "Jellyfin's account can't sign in: it answered $said"
  said=$(jellyfin_sign_in "$port" not-the-password) || fail "Jellyfin's sign-in got no answer"
  [[ "$said" == 401 ]] || fail "Jellyfin answered $said to a wrong password, want 401"
}
