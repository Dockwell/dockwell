#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Lays out a mirror for the install tests (run.sh), as a mirror for mainland
# China would hold it: Dockwell's releases and cosign's, each in GitHub's
# layout, to serve at a URL other than the releases' own:
#
#   <out>/good/, <out>/bad-archive/, <out>/bad-signature/
#                       the test releases from releases.sh
#   <out>/bad-root/     v<new> with a Sigstore trust root that isn't the pinned one
#   <out>/no-root/      v<new> without a trust root, as a mirror that re-signs
#                       releases with its own key may serve it
#   <out>/old-root/     checksums and trust roots only, where v<old> has no trust
#                       root, as a release made before mirrors were supported
#   <out>/latest-root/  checksums and trust roots only, where v<new> has a trust
#                       root that isn't the pinned one, and latest/download the
#                       pinned one
#   <out>/cosign/good/download/<version>/cosign-linux-<arch>
#                       the cosign install.sh pins
#   <out>/cosign/bad/download/<version>/cosign-linux-<arch>
#                       a cosign that doesn't match its pin
#   <out>/keyless/      cosign's own release checksums and their bundle, signed
#                       keyless on Sigstore's public instance, to verify offline
#
#   scripts/install-test/mirror.sh <releases> <cosign> <out>
#
# <cosign> is the cosign install.sh pins, for this machine's architecture.
set -euo pipefail

releases=$(realpath "${1:?usage: mirror.sh <releases> <cosign> <out>}")
cosign=$(realpath "${2:?usage: mirror.sh <releases> <cosign> <out>}")
out=${3:?usage: mirror.sh <releases> <cosign> <out>}
install_sh="$(cd "$(dirname "$0")/.." && pwd)/install.sh"
# shellcheck source=/dev/null # written by releases.sh
source "$releases/versions"
cosign_version=$(sed -n 's/^COSIGN_VERSION=//p' "$install_sh")
cosign_releases=$(sed -n 's/^COSIGN_RELEASES=//p' "$install_sh")

case $(uname -m) in
x86_64) arch=amd64 ;;
aarch64) arch=arm64 ;;
*)
  echo "mirror.sh: $(uname -m) isn't an architecture install.sh supports." >&2
  exit 1
  ;;
esac

rm -rf "$out"
mkdir -p "$out"
for tree in good bad-archive bad-signature; do
  cp -R "$releases/$tree" "$out/$tree"
done

# A release whose trust root was changed on the mirror.
bad="$out/bad-root/download/v$NEW"
mkdir -p "$bad"
cp "$releases/good/download/v$NEW"/* "$bad/"
printf ' ' >>"$bad/sigstore_trusted_root.json"
mkdir -p "$out/bad-root/latest" && cp -R "$bad" "$out/bad-root/latest/download"

# A release that a mirror re-signed, with no trust root, since a key needs none.
bare="$out/no-root/download/v$NEW"
mkdir -p "$bare"
cp "$releases/good/download/v$NEW"/* "$bare/"
rm "$bare/sigstore_trusted_root.json"
mkdir -p "$out/no-root/latest" && cp -R "$bare" "$out/no-root/latest/download"

# roots copies what install.sh reads to find the trust root, and no archives:
# each release's checksums and trust root, and the latest release's.
roots() {
  local dir
  for dir in "download/v$OLD" "download/v$NEW" latest/download; do
    mkdir -p "$out/$1/$dir"
    cp "$releases/good/$dir/checksums.txt" "$releases/good/$dir/sigstore_trusted_root.json" "$out/$1/$dir/"
  done
}
roots old-root
rm "$out/old-root/download/v$OLD/sigstore_trusted_root.json"
roots latest-root
printf ' ' >>"$out/latest-root/download/v$NEW/sigstore_trusted_root.json"

for tree in good bad; do
  mkdir -p "$out/cosign/$tree/download/$cosign_version"
  cp "$cosign" "$out/cosign/$tree/download/$cosign_version/cosign-linux-$arch"
done
printf 'tampered' >>"$out/cosign/bad/download/$cosign_version/cosign-linux-$arch"

mkdir -p "$out/keyless"
for file in cosign_checksums.txt cosign_checksums.txt.sigstore.json; do
  curl -fsSL --retry 3 -o "$out/keyless/$file" "$cosign_releases/download/$cosign_version/$file"
done
echo "mirror: v$OLD, v$NEW and cosign $cosign_version for linux/$arch in $out"
