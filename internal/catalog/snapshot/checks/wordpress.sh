# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# wordpress's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_wordpress first, once it's installed,
# and check_wordpress upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[wordpress]="7.1.2 wordpress:7.1.2-apache@sha256:e04cf657f24889e96b5544828cdd5a92a0b51819f3e1899183f9be6ea0485edf"

# wordpress_sign_in signs in to WordPress, and says how it answered and where it sends
# you: its dashboard, or nowhere, as its login page says why on the page itself.
wordpress_sign_in() {
  local jar landed
  jar=$(mktemp)
  curl -fsS --max-time 15 -o /dev/null -c "$jar" "http://127.0.0.1:$1/wp-login.php" || fail "WordPress's login page doesn't answer"
  landed=$(curl -sS --max-time 30 -o /dev/null -w '%{http_code} %{redirect_url}' -b "$jar" -c "$jar" --data-urlencode log=owner --data-urlencode "pwd=$2" \
    --data-urlencode 'wp-submit=Log In' --data-urlencode testcookie=1 --data-urlencode "redirect_to=http://127.0.0.1:$1/wp-admin/" \
    "http://127.0.0.1:$1/wp-login.php") || fail "signing in to WordPress got no answer"
  rm -f "$jar"
  echo "$landed"
}

check_wordpress() {
  local port said
  port=$(value PORT)
  if [[ "$1" == first ]]; then
    said=$(curl -fsS --max-time 60 --data-urlencode 'weblog_title=E2E kept' --data-urlencode user_name=owner --data-urlencode admin_password=E2e-owner-passw0rd-long \
      --data-urlencode admin_password2=E2e-owner-passw0rd-long --data-urlencode admin_email=owner@example.com --data-urlencode blog_public=0 \
      --data-urlencode language= --data-urlencode 'Submit=Install WordPress' "http://127.0.0.1:$port/wp-admin/install.php?step=2") || fail "WordPress's install doesn't answer"
    [[ "$said" == *wp-login.php* ]] || fail "WordPress didn't install: ${said:0:600}"
  fi
  said=$(wordpress_sign_in "$port" E2e-owner-passw0rd-long) || fail "signing in to WordPress failed"
  [[ "$said" == 302\ */wp-admin/ ]] || fail "WordPress's admin can't sign in: it answered $said"
  said=$(wordpress_sign_in "$port" not-the-password) || fail "signing in to WordPress failed"
  [[ "$said" == "200 " ]] || fail "WordPress didn't refuse a wrong password: it answered $said"
  said=$(curl -fsS --max-time 15 "http://127.0.0.1:$port/?rest_route=/") || fail "WordPress's API doesn't answer"
  [[ "$(jq -r .name <<<"$said")" == "E2E kept" ]] || fail "WordPress's site reads ${said:0:300}"
}
