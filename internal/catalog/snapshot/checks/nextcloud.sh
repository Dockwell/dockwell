# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# nextcloud's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_nextcloud first, once it's installed,
# and check_nextcloud upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[nextcloud]="35.0.1 nextcloud:35.0.1-apache@sha256:8f2f0e93aca5e9457019933936dea1f313052e72c696b4aecb31580a208b859f"

# What a person gives its install, as dockwell catalog install takes it.
given[nextcloud]="DOMAIN=cloud.example.com"

check_nextcloud() {
  local port password said
  port=$(value PORT)
  password=$(generated nextcloud NEXTCLOUD_ADMIN_PASSWORD)
  # As localhost, as an SSH tunnel reaches it: it answers only at what it trusts.
  nc_curl() { curl -H 'Host: localhost' "$@"; }
  said=$(nc_curl -fsS --max-time 15 "http://127.0.0.1:$port/status.php") || fail "Nextcloud doesn't answer on port $port"
  jq -e --arg v "$version" '.installed == true and .versionstring == $v and .maintenance == false' <<<"$said" >/dev/null ||
    fail "Nextcloud says $said, want $version installed"
  nextcloud_user() {
    nc_curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -u "admin:$1" -H 'OCS-APIRequest: true' "http://127.0.0.1:$port/ocs/v2.php/cloud/user?format=json"
  }
  said=$(nextcloud_user "$password") || fail "Nextcloud's sign-in got no answer"
  [[ "$said" == 200 ]] || fail "Nextcloud doesn't sign in with the password Dockwell generated: it answered $said"
  if [[ "$1" == first ]]; then
    nc_curl -fsS --max-time 15 -u "admin:$password" -X PUT --data-binary kept "http://127.0.0.1:$port/remote.php/dav/files/admin/e2e.txt" ||
      fail "Nextcloud doesn't take a file"
  fi
  said=$(nc_curl -fsS --max-time 15 -u "admin:$password" "http://127.0.0.1:$port/remote.php/dav/files/admin/e2e.txt") || fail "Nextcloud doesn't give back the file it took"
  [[ "$said" == kept ]] || fail "Nextcloud gave back '$said'"
  docker exec -u www-data "$(container nextcloud)" php occ config:system:get trusted_domains | grep -qx cloud.example.com ||
    fail "Nextcloud doesn't answer at the domain it was given"
  # Last, as a wrong password slows what comes after it from the same address.
  said=$(nextcloud_user not-the-password) || fail "Nextcloud's sign-in got no answer"
  [[ "$said" == 401 ]] || fail "Nextcloud answered $said to a wrong password"
}
