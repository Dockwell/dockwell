# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# linkding's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_linkding first, once it's installed,
# and check_linkding upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[linkding]="1.46.2 sissbruecker/linkding:1.46.2@sha256:0c0a9a04c7ebd644293c37f98710872fe7cdff0ea1ff7fd9be4d235e4c4678d2"

# A real browser login proves the generated password works. The API is reached with
# the same session and CSRF cookie, so no token needs to be read from its database.
check_linkding() {
  local port base password jar page csrf status said wrong
  port=$(value PORT)
  base="http://127.0.0.1:$port"
  password=$(generated linkding LD_SUPERUSER_PASSWORD)
  jar=$(mktemp)
  page=$(curl -fsS --max-time 15 -c "$jar" "$base/login/") || fail "linkding's login page doesn't answer"
  csrf=$(sed -n 's/.*name="csrfmiddlewaretoken" value="\([^"]*\)".*/\1/p' <<<"$page" | head -n 1)
  [[ -n "$csrf" ]] || fail "linkding's login has no CSRF token"
  # A wrong password gets the login form back and no session. linkding answers it
  # with 401, as its 1.46.2 image did in CI, where Django's own view says 200. What
  # counts is that it's the form again, and that the API still refuses the session.
  wrong=$(mktemp)
  status=$(curl -sS --max-time 15 -b "$jar" -c "$jar" -o "$wrong" -w '%{http_code}' \
    --data-urlencode "csrfmiddlewaretoken=$csrf" --data-urlencode username=admin --data-urlencode password=not-the-password "$base/login/") || fail "linkding's sign-in doesn't answer"
  [[ "$status" == 401 || "$status" == 200 ]] || fail "linkding answers $status to a wrong password, want its login form again, with 401 or 200"
  grep -q 'name="csrfmiddlewaretoken"' "$wrong" || fail "linkding's answer to a wrong password isn't its login form"
  rm -f "$wrong"
  status=$(curl -sS --max-time 10 -b "$jar" -o /dev/null -w '%{http_code}' "$base/api/bookmarks/") || fail "linkding's API doesn't answer"
  [[ "$status" == 401 || "$status" == 403 ]] || fail "linkding's API answers $status after a wrong password, want it refused"
  status=$(curl -sS --max-time 15 -b "$jar" -c "$jar" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrfmiddlewaretoken=$csrf" --data-urlencode username=admin --data-urlencode "password=$password" "$base/login/") || fail "linkding's sign-in doesn't answer"
  [[ "$status" == 302 ]] || fail "linkding answers $status to its generated password, want a signed-in redirect"
  # linkding names its CSRF cookie ld_csrftoken, and rotates it as you sign in.
  csrf=$(awk '$6 == "ld_csrftoken" { print $7 }' "$jar" | tail -n 1)
  [[ -n "$csrf" ]] || fail "linkding's signed-in session has no ld_csrftoken cookie"
  if [[ "$1" == first ]]; then
    # This user's test fixture doesn't fetch icons, previews or archives from the
    # Internet. The bookmark itself goes through the signed-in public API.
    docker exec "$(container linkding)" python manage.py shell -c \
      'from django.contrib.auth import get_user_model; from bookmarks.models import UserProfile; p=get_user_model().objects.get(username="admin").profile; p.web_archive_integration=UserProfile.WEB_ARCHIVE_INTEGRATION_DISABLED; p.enable_favicons=False; p.enable_preview_images=False; p.enable_automatic_html_snapshots=False; p.save()' >/dev/null || fail "can't turn off external bookmark loading for this fixture"
    said=$(curl -fsS --max-time 15 -b "$jar" -H "X-CSRFToken: $csrf" -H 'Content-Type: application/json' \
      -d '{"url":"https://example.invalid/catalog-e2e","title":"Kept through upgrade","description":"catalog-e2e"}' "$base/api/bookmarks/?disable_scraping&disable_html_snapshot") || fail "linkding doesn't save a bookmark"
    kept[linkding-bookmark]=$(jq -er .id <<<"$said") || fail "linkding saved no bookmark ID"
  fi
  said=$(curl -fsS --max-time 10 -b "$jar" "$base/api/bookmarks/${kept[linkding-bookmark]}/") || fail "linkding can't read its saved bookmark"
  jq -e '.url == "https://example.invalid/catalog-e2e" and .title == "Kept through upgrade" and .description == "catalog-e2e"' <<<"$said" >/dev/null || fail "linkding's bookmark didn't stay through upgrade"
  rm -f "$jar"
}
