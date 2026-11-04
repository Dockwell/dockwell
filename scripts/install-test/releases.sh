#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Lays out test releases for scripts/install.sh in GitHub's URL layout, signed
# with a throwaway cosign key, so the install tests need no published release:
#
#   <out>/good/download/v<new>/        the snapshot from make release-snapshot,
#                                      with install.sh and Sigstore's trust root
#   <out>/good/download/v0.0.0-test/   an older build, to upgrade from
#   <out>/good/latest/download/        the same files as v<new>
#   <out>/bad-archive/...              v<new> with an archive that doesn't match its checksum
#   <out>/bad-signature/...            v<new> with checksums that don't match their signature
#   <out>/cosign.pub                   the key that signed them all
#   <out>/versions                     OLD=0.0.0-test and NEW=<new>
#
#   scripts/install-test/releases.sh <dist> <old> <out> [cosign]
#
# <dist> is GoReleaser's output. <old> holds linux_amd64/dockwell and
# linux_arm64/dockwell built with an older version (make install-test-releases
# builds both). cosign defaults to the one on PATH.
set -euo pipefail

dist=${1:?usage: releases.sh <dist> <old> <out> [cosign]}
old=${2:?usage: releases.sh <dist> <old> <out> [cosign]}
out=${3:?usage: releases.sh <dist> <old> <out> [cosign]}
cosign=${4:-cosign}
old_version=0.0.0-test
scripts=$(cd "$(dirname "$0")/.." && pwd)

new_version=$(sed -n 's/^[0-9a-f]\{64\}  dockwell_\(.*\)_linux_amd64\.tar\.gz$/\1/p' "$dist/checksums.txt")
[[ -n "$new_version" ]] || {
  echo "releases: $dist/checksums.txt lists no linux/amd64 archive. Run make release-snapshot first." >&2
  exit 1
}

rm -rf "$out"
mkdir -p "$out"
export COSIGN_PASSWORD=""
"$cosign" generate-key-pair --output-key-prefix "$out/cosign" >/dev/null 2>&1

# sign writes checksums.txt.sigstore.json next to a checksums.txt.
sign() {
  "$cosign" sign-blob --yes --key "$out/cosign.key" --use-signing-config=false --tlog-upload=false \
    --bundle "$1/checksums.txt.sigstore.json" "$1/checksums.txt" >/dev/null 2>&1
}

good="$out/good"
new_dir="$good/download/v$new_version"
mkdir -p "$new_dir"
cp "$dist"/dockwell_"$new_version"_linux_*.tar.gz "$dist/checksums.txt" "$new_dir/"
# A release carries the install script and the trust root it pins, as
# .github/workflows/release.yml attaches them.
cp "$scripts/install.sh" "$scripts/sigstore_trusted_root.json" "$new_dir/"
sign "$new_dir"

old_dir="$good/download/v$old_version"
mkdir -p "$old_dir"
for arch in amd64 arm64; do
  stage=$(mktemp -d)
  install -m 0755 "$old/linux_$arch/dockwell" "$stage/dockwell"
  tar -czf "$old_dir/dockwell_${old_version}_linux_$arch.tar.gz" -C "$stage" dockwell
  rm -rf "$stage"
done
cp "$scripts/install.sh" "$scripts/sigstore_trusted_root.json" "$old_dir/"
(cd "$old_dir" && sha256sum dockwell_*.tar.gz install.sh sigstore_trusted_root.json >checksums.txt)
sign "$old_dir"

mkdir -p "$good/latest"
cp -R "$new_dir" "$good/latest/download"

# A release whose archive was swapped after signing.
bad="$out/bad-archive/download/v$new_version"
mkdir -p "$bad"
cp "$new_dir"/* "$bad/"
for archive in "$bad"/*.tar.gz; do printf 'tampered' >>"$archive"; done
mkdir -p "$out/bad-archive/latest" && cp -R "$bad" "$out/bad-archive/latest/download"

# A release whose checksums were edited after signing, to match a swapped archive.
bad="$out/bad-signature/download/v$new_version"
mkdir -p "$bad"
cp "$new_dir"/* "$bad/"
for archive in "$bad"/*.tar.gz; do printf 'tampered' >>"$archive"; done
(cd "$bad" && sha256sum dockwell_*.tar.gz >checksums.txt)
mkdir -p "$out/bad-signature/latest" && cp -R "$bad" "$out/bad-signature/latest/download"

printf 'OLD=%s\nNEW=%s\n' "$old_version" "$new_version" >"$out/versions"
echo "releases: v$old_version and v$new_version in $out, signed with $out/cosign.pub"
