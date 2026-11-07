#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Fetches what tests the ingress against the real thing into a directory: Caddy
# at the version the agent pins, and Pebble, Let's Encrypt's test CA, with the
# DNS server it asks. Each is checked against the SHA-256 written here, and
# fetched only if it isn't there yet. It prints the directory.
#
#   scripts/ingress-tools.sh path/to/dir
set -euo pipefail

dir=${1:?usage: scripts/ingress-tools.sh path/to/dir}
mkdir -p "$dir"
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *)
    echo "ingress-tools: there's no Caddy or Pebble pinned for $(uname -m). Add them to scripts/ingress-tools.sh" >&2
    exit 1
    ;;
esac

# fetch downloads an archive, checks it, and takes one file from it.
fetch() {
  local name=$1 url=$2 sum=$3 member=$4 tmp
  [[ -x "$dir/$name" ]] && return 0
  tmp=$(mktemp -d)
  if ! curl -fsSL --retry 6 --retry-delay 15 --retry-max-time 120 --retry-all-errors -o "$tmp/archive.tar.gz" "$url"; then
    rm -rf "$tmp"
    echo "ingress-tools: can't download $url" >&2
    return 1
  fi
  if ! echo "$sum  $tmp/archive.tar.gz" | sha256sum -c - >/dev/null; then
    rm -rf "$tmp"
    echo "ingress-tools: $url doesn't match the SHA-256 in this script, so it wasn't used" >&2
    return 1
  fi
  tar -xzf "$tmp/archive.tar.gz" -C "$tmp" "$member"
  install -m 0755 "$tmp/$member" "$dir/$name"
  rm -rf "$tmp"
}

caddy_version=2.11.7
pebble_version=2.10.1
case "$arch" in
  amd64)
    caddy_sum=727b91701a392de6ebc5027509f548bf39979e5216340d0faed8fa5e69c84f8b
    pebble_sum=4f2fcb5bca8c85c9cf73ad140fccfc0d2be40bd81ab99879c79b7b8a0b4f70ed
    challtest_sum=e93a5aa25ecdf3af2f9fbb2de32b0173e64a2eae81002a4ccfe35fa6f4f60b92
    ;;
  arm64)
    caddy_sum=d8fc6d179a5d283028a472a5618564f6ad8a86fed513e64f032b3b0b7cc45e42
    pebble_sum=b53fd072a69eb7692451de4e8b0667e0bdf5cccd7e36fc51b8eaf2fcc135ed9f
    challtest_sum=db8e1a79ccdb2195c489fbe4f40fddb7f30e86f9cd8a07912566ee5025094d6c
    ;;
esac

fetch caddy "https://github.com/caddyserver/caddy/releases/download/v$caddy_version/caddy_${caddy_version}_linux_$arch.tar.gz" "$caddy_sum" caddy
fetch pebble "https://github.com/letsencrypt/pebble/releases/download/v$pebble_version/pebble-linux-$arch.tar.gz" "$pebble_sum" "pebble-linux-$arch/linux/$arch/pebble"
fetch pebble-challtestsrv "https://github.com/letsencrypt/pebble/releases/download/v$pebble_version/pebble-challtestsrv-linux-$arch.tar.gz" "$challtest_sum" "pebble-challtestsrv-linux-$arch/linux/$arch/pebble-challtestsrv"
realpath "$dir"
