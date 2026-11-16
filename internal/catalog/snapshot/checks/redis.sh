# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# redis's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_redis first, once it's installed,
# and check_redis upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[redis]="8.10.1 redis:8.10.1@sha256:8a1efc5f479551822b47424ccae982026b633f28818eab0387348120a61e10e2"

check_redis() {
  local redis port password said bound
  redis=$(container redis)
  port=$(value PORT)
  password=$(generated redis REDISCLI_AUTH)
  bound=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$redis") || fail "docker inspect $redis failed"
  jq -e '[.[][] | .HostIp] == ["127.0.0.1"]' <<<"$bound" >/dev/null || fail "Redis is published past this server: $bound"
  # On its port, as an app on this server reaches it, in Redis's own protocol.
  said=$(exec 3<>"/dev/tcp/127.0.0.1/$port" && printf 'AUTH %s\r\nPING\r\n' "$password" >&3 && timeout 5 head -c 12 <&3) || fail "Redis doesn't answer on port $port"
  [[ "$said" == *"+OK"*"+PONG"* ]] || fail "Redis answered '${said//$'\r'/}' to its password"
  # redis-cli exits 0 or not as it likes on a refusal, so what it says decides.
  said=$(docker exec -e REDISCLI_AUTH=not-the-password "$redis" redis-cli ping 2>&1) || true
  [[ "$said" == *WRONGPASS* || "$said" == *NOAUTH* ]] || fail "Redis didn't refuse a wrong password: ${said:0:200}"
  if [[ "$1" == first ]]; then
    [[ "$(docker exec "$redis" redis-cli set e2e kept)" == OK ]] || fail "Redis doesn't keep a value"
  else
    [[ "$(docker exec "$redis" redis-cli get e2e)" == kept ]] || fail "what Redis kept didn't stay through the upgrade"
  fi
}
