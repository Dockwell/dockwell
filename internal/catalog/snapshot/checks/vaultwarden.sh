# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# vaultwarden's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_vaultwarden first, once it's installed,
# and check_vaultwarden upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[vaultwarden]="1.37.3 vaultwarden/server:1.37.3@sha256:1587c45feaa479f1f5e8af3b00eded36bff77bcf1880cf8dbf0541706dd470e0"

# vaultwarden_sign_in signs in to Vaultwarden as Bitwarden's apps do, with what stands
# for the master password's hash, and prints the status and what it answered.
vaultwarden_sign_in() {
  curl -sS --max-time 15 -w '\n%{http_code}' --data-urlencode grant_type=password --data-urlencode username=owner@example.com \
    --data-urlencode "password=$2" --data-urlencode 'scope=api offline_access' --data-urlencode client_id=web --data-urlencode deviceType=10 \
    --data-urlencode deviceIdentifier=0d5b4c2e-7c3e-4b6a-9f7a-1e2d3c4b5a69 --data-urlencode deviceName=e2e "http://127.0.0.1:$1/identity/connect/token"
}

check_vaultwarden() {
  local port said token hash=ZTJlLW1hc3Rlci1wYXNzd29yZC1oYXNoLXN0YW5kLWlu
  port=$(value PORT)
  curl -fsS --max-time 10 -o /dev/null "http://127.0.0.1:$port/alive" || fail "Vaultwarden doesn't answer on port $port"
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/config") || fail "Vaultwarden's API doesn't answer"
  [[ "$(jq -r .server.name <<<"$said")" == Vaultwarden ]] || fail "port $port answers something else: ${said:0:300}"
  if [[ "$1" == first ]]; then
    # With no mail server, it gives the token a verification email would carry.
    token=$(curl -fsS --max-time 15 -H 'Content-Type: application/json' -H 'Accept: application/json' \
      -d '{"email":"owner@example.com","name":"Owner"}' "http://127.0.0.1:$port/identity/accounts/register/send-verification-email") ||
      fail "Vaultwarden doesn't take a new account"
    token=$(jq -r . <<<"$token")
    said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg t "$token" --arg h "$hash" '{email: "owner@example.com", masterPasswordHash: $h, emailVerificationToken: $t, kdf: 0, kdfIterations: 600000,
        userSymmetricKey: "2.AAAAAAAAAAAAAAAAAAAAAA==|AAAAAAAAAAAAAAAAAAAAAA==|AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}')" \
      "http://127.0.0.1:$port/identity/accounts/register/finish") || fail "Vaultwarden's registration got no answer"
    [[ "$said" == 200 ]] || fail "Vaultwarden didn't make the account: it answered $said"
  fi
  # The account is in its database, so it signs in after an upgrade too.
  said=$(vaultwarden_sign_in "$port" "$hash") || fail "Vaultwarden's sign-in got no answer"
  [[ "${said##*$'\n'}" == 200 && -n "$(jq -r '.access_token // empty' <<<"${said%$'\n'*}")" ]] || fail "Vaultwarden's account can't sign in: ${said:0:300}"
  said=$(vaultwarden_sign_in "$port" bm90LXRoZS1wYXNzd29yZA==) || fail "Vaultwarden's sign-in got no answer"
  [[ "${said##*$'\n'}" == 400 && "$said" == *"Username or password is incorrect"* && "$said" != *access_token* ]] ||
    fail "Vaultwarden didn't refuse a wrong password: ${said:0:300}"
}
