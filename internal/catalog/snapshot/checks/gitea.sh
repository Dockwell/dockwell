# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# gitea's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_gitea first, once it's installed,
# and check_gitea upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[gitea]="28.1.0 gitea/gitea:28.1.0@sha256:f505a0d3a41323b786e05942fc7a4075e07ae7c169fd9485649aa55ee543a21a"

check_gitea() {
  local gitea port said banner key
  gitea=$(container gitea)
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/v1/version") || fail "Gitea doesn't answer on port $port"
  [[ "$(jq -r .version <<<"$said")" == "$version" ]] || fail "Gitea says it's $said, want $version"
  # It encrypts with the key Dockwell generated, not the one its source publishes.
  key=$(docker exec "$gitea" sed -n 's/^SECRET_KEY *= *//p' /data/gitea/conf/app.ini) || fail "Gitea has no app.ini"
  [[ ${#key} -ge 64 && "$key" == "$(env_of "$gitea" GITEA__security__SECRET_KEY)" ]] || fail "Gitea's SECRET_KEY isn't the one Dockwell generated"
  if [[ "$1" == first ]]; then
    docker exec -u git "$gitea" gitea admin user create --username owner --password E2e-owner-passw0rd --email owner@example.com --admin --must-change-password=false >/dev/null ||
      fail "Gitea can't make a user"
  fi
  # What it keeps is on its volume, so the user signs in after an upgrade too.
  said=$(curl -fsS --max-time 10 -u owner:E2e-owner-passw0rd "http://127.0.0.1:$port/api/v1/user") || fail "Gitea's user can't sign in"
  [[ "$(jq -r .login <<<"$said")" == owner ]] || fail "Gitea signed in as $said"
  said=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -u owner:not-the-password "http://127.0.0.1:$port/api/v1/user") || fail "Gitea's sign-in got no answer"
  [[ "$said" == 401 ]] || fail "Gitea answered $said to a wrong password"
  banner=$(exec 3<>"/dev/tcp/127.0.0.1/$(value SSH_PORT)" && timeout 5 head -c 7 <&3) || fail "Gitea's SSH doesn't answer"
  [[ "$banner" == SSH-2.0 ]] || fail "Gitea's SSH port answers '$banner'"
}
