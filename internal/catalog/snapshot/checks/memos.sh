# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# memos's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_memos first, once it's installed,
# and check_memos upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[memos]="0.30.0 neosmemo/memos:0.30.0@sha256:71a5b4738d1bed96e92112004054f0888e92791b64eb78afd79077c96e6f9327"

check_memos() {
  local port said token
  port=$(value PORT)
  said=$(curl -fsS --max-time 10 "http://127.0.0.1:$port/api/v1/instance/profile") || fail "Memos doesn't answer on port $port"
  [[ "$(jq -r .version <<<"$said")" == "$version" ]] || fail "Memos says $said, want $version"
  if [[ "$1" == first ]]; then
    said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
      -d '{"username":"owner","password":"E2e-owner-passw0rd"}' "http://127.0.0.1:$port/api/v1/users") || fail "Memos's first user got no answer"
    [[ "$said" == 200 ]] || fail "Memos didn't make its first user: it answered $said"
  fi
  memos_sign_in() {
    curl -sS --max-time 15 -H 'Content-Type: application/json' \
      -d "{\"passwordCredentials\":{\"username\":\"owner\",\"password\":\"$1\"}}" "http://127.0.0.1:$port/api/v1/auth/signin"
  }
  # The right password gives a token, and the token says who it signed in.
  said=$(memos_sign_in E2e-owner-passw0rd) || fail "Memos's sign-in got no answer"
  token=$(jq -r '.accessToken // empty' <<<"$said" 2>/dev/null)
  [[ -n "$token" ]] || fail "Memos's admin can't sign in: ${said:0:300}"
  said=$(curl -sS --max-time 15 -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/v1/auth/me") || fail "Memos's current user got no answer"
  [[ "$(jq -r '.user.username // empty' <<<"$said" 2>/dev/null)" == owner ]] || fail "Memos's token doesn't sign in as owner: ${said:0:300}"
  # Memos 0.30 and 0.31 refuse a wrong password as InvalidArgument, which its
  # gateway answers with 400, saying "unmatched username and password", and no token.
  said=$(memos_sign_in not-the-password) || fail "Memos's sign-in got no answer"
  jq -e '(.accessToken // "") == "" and (.message // "" | contains("unmatched username and password"))' <<<"$said" >/dev/null 2>&1 ||
    fail "Memos didn't refuse a wrong password: ${said:0:300}"
}
