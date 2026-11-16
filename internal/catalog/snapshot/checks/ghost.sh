# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# ghost's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_ghost first, once it's installed,
# and check_ghost upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# The release before, by its version and each image by digest, which an upgrade
# is proven from where the catalog's history holds none of another version.
before[ghost]="6.71.0 ghost:6.71.0@sha256:00032f15726204565bf799ac272ee6fef588547cce50bedd38de3db55272c053"

# What a person gives its install, as dockwell catalog install takes it.
given[ghost]="URL=https://blog.example.com"

# ghost_get asks Ghost as the https proxy in front of it does.
ghost_get() {
  curl -fsS --max-time 15 -H 'X-Forwarded-Proto: https' "http://127.0.0.1:$1$2"
}

check_ghost() {
  local ghost port said reported
  ghost=$(container ghost)
  port=$(value PORT)
  # It's given an https address, as behind a domain, so it answers what the proxy in front
  # sends, and sends anything else there. Its health check asks as that proxy does.
  said=$(curl -sS --max-time 15 -o /dev/null -w '%{http_code} %{redirect_url}' "http://127.0.0.1:$port/ghost/api/admin/site/") || fail "Ghost doesn't answer on port $port"
  [[ "$said" == 30?\ https://blog.example.com/ghost/* ]] || fail "Ghost answers '$said' to plain http, want it sent to https://blog.example.com"
  said=$(ghost_get "$port" /ghost/api/admin/site/) || fail "Ghost doesn't answer on port $port behind an https proxy"
  # It says its version without the patch, and its image says the whole of it.
  reported=$(jq -r '.site.version // empty' <<<"$said")
  [[ -n "$reported" && "$reported" == "${version%.*}" && "$(env_of "$ghost" GHOST_VERSION)" == "$version" ]] ||
    fail "Ghost says it's '$reported' and its image '$(env_of "$ghost" GHOST_VERSION)', want $version"
  [[ "$(env_of "$ghost" url)" == https://blog.example.com ]] || fail "Ghost's url is '$(env_of "$ghost" url)', want the address it was given"
  if [[ "$1" == first ]]; then
    # Ghost refuses a password with a common word in it, such as passw0rd, so this has none.
    said=$(curl -sS --max-time 30 -w '\n%{http_code}' -H 'X-Forwarded-Proto: https' -H 'Content-Type: application/json' -H 'Origin: https://blog.example.com' \
      -d '{"setup":[{"name":"Owner","email":"owner@example.com","password":"Kx7-Vq2!mRt9-Lw4zB","blogTitle":"E2E kept"}]}' \
      "http://127.0.0.1:$port/ghost/api/admin/authentication/setup/") || fail "Ghost's setup got no answer"
    [[ "${said##*$'\n'}" == 201 ]] || fail "Ghost didn't make its owner: it answered ${said##*$'\n'}, saying ${said%$'\n'*}"
  fi
  # What it keeps is in MySQL, so the site keeps its title through an upgrade.
  said=$(ghost_get "$port" /ghost/api/admin/site/) || fail "Ghost doesn't answer on port $port behind an https proxy"
  [[ "$(jq -r .site.title <<<"$said")" == "E2E kept" ]] || fail "Ghost's site reads $said"
}
