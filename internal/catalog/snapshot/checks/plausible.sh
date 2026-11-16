# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# plausible's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_plausible first, once it's installed,
# and check_plausible upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[plausible]="3.2.0 ghcr.io/plausible/community-edition:v3.2.0@sha256:3d8af8d4a1be32c3dcc5e8dc01496639d945edd9963827e18abe984dd6bda086"

check_plausible() {
  local plausible port said db events migrations tables jar token landed
  plausible=$(container plausible)
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/system/health/ready") || fail "Plausible isn't ready on port $port"
  jq -e . <<<"$said" >/dev/null || fail "Plausible's readiness says what isn't JSON: ${said:0:300}"
  db=$(container db)
  events=$(container events)
  migrations=$(docker exec "$db" psql -X -U plausible -d plausible -tAc 'SELECT count(*) FROM schema_migrations') || fail "Plausible's PostgreSQL has no migrations"
  tables=$(docker exec "$events" clickhouse-client -q "SELECT count() FROM system.tables WHERE database = 'plausible_events_db'") || fail "Plausible's ClickHouse doesn't answer"
  ((migrations > 0 && tables > 0)) || fail "Plausible has $migrations migrations in PostgreSQL and $tables tables in ClickHouse"
  said=$(docker exec "$events" clickhouse-client -q "SELECT count() FROM system.tables WHERE database = 'system' AND name = 'trace_log'") || fail "Plausible's ClickHouse doesn't answer"
  [[ "$said" == 0 ]] || fail "ClickHouse keeps a trace log, which Plausible's settings turn off"
  if [[ "$1" == first ]]; then
    # On first launch it leads to registration.
    said=$(curl -sS --max-time 10 -o /dev/null -w '%{redirect_url}' "http://127.0.0.1:$port/") || fail "Plausible doesn't answer on port $port"
    [[ "$said" == */register ]] || fail "Plausible's first page leads to '$said', want its registration"
    # Its registration is a live page, which curl can't drive, so its own release makes the account.
    docker exec "$plausible" /app/bin/plausible rpc \
      'Plausible.Repo.insert!(Plausible.Auth.User.new(%{"name" => "Owner", "email" => "owner@example.com", "password" => "Zq7-Tm2!vLx9-Pw4rK", "password_confirmation" => "Zq7-Tm2!vLx9-Pw4rK"}))' >/dev/null ||
      fail "Plausible's release can't make an account"
  fi
  # The account is in PostgreSQL, so it signs in after an upgrade too.
  plausible_sign_in() {
    jar=$(mktemp)
    said=$(curl -fsS --max-time 15 -c "$jar" -b "$jar" "http://127.0.0.1:$port/login") || fail "Plausible's login page doesn't answer"
    token=$(sed -n 's/.*name="_csrf_token"[^>]*value="\([^"]*\)".*/\1/p' <<<"$said" | head -1)
    [[ -n "$token" ]] || fail "Plausible's login page has no token: ${said:0:300}"
    landed=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code} %{redirect_url}' -c "$jar" -b "$jar" --data-urlencode "_csrf_token=$token" \
      --data-urlencode email=owner@example.com --data-urlencode "password=$1" "http://127.0.0.1:$port/login") || fail "signing in to Plausible got no answer"
    rm -f "$jar"
    echo "$landed"
  }
  landed=$(plausible_sign_in 'Zq7-Tm2!vLx9-Pw4rK') || fail "signing in to Plausible failed"
  [[ "$landed" == 302\ */sites* ]] || fail "Plausible's account can't sign in: it answered $landed"
  landed=$(plausible_sign_in not-the-password) || fail "signing in to Plausible failed"
  [[ "$landed" == "200 " ]] || fail "Plausible didn't refuse a wrong password: it answered $landed"
  if [[ "$1" == first ]]; then kept[plausible]=$tables; fi
  ((tables >= kept[plausible])) || fail "Plausible's ClickHouse had ${kept[plausible]} tables before the upgrade, and has $tables"
}
