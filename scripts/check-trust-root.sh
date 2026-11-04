#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Checks the copy of Sigstore's trust root that install.sh pins for installs
# through a mirror (docs/adr/0013-mirrors-for-releases-and-images.md):
#
#   scripts/check-trust-root.sh pin
#       install.sh pins scripts/sigstore_trusted_root.json by its SHA-256.
#   scripts/check-trust-root.sh sigstore
#       The pin, and that the file is the trust root Sigstore publishes through
#       TUF now, and that keyless signatures verify against it with the network
#       cut off: cosign's own release, and Dockwell's latest once there is one.
#
# sigstore needs the cosign install.sh pins on PATH, curl, gh (with GH_TOKEN in
# CI), and sudo, to cut cosign off in a network namespace of its own. The
# release workflow runs pin before it builds and sigstore before it publishes;
# .github/workflows/trust-root.yml runs sigstore nightly, and on the pull
# requests that change what it checks.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
root="$PWD/scripts/sigstore_trusted_root.json"
repo=${GITHUB_REPOSITORY:-Dockwell/dockwell}
server=${GITHUB_SERVER_URL:-https://github.com}

# fail reports a problem and stops, as an annotation on the file in $1 when
# GitHub Actions runs this.
fail() {
  if [[ -n ${GITHUB_ACTIONS:-} ]]; then
    echo "::error${1:+ file=$1}::$2"
  else
    echo "check-trust-root.sh: $2" >&2
  fi
  exit 1
}

# note says what was checked, in the job summary too when there is one.
note() {
  echo "$1"
  [[ -z ${GITHUB_STEP_SUMMARY:-} ]] || echo "$1" >>"$GITHUB_STEP_SUMMARY"
}

check_pin() {
  local pinned actual
  pinned=$(sed -n 's/^SIGSTORE_ROOT_SHA256=//p' scripts/install.sh)
  actual=$(sha256sum "$root" | cut -d ' ' -f 1)
  [[ -n $pinned && $pinned == "$actual" ]] ||
    fail scripts/install.sh "install.sh pins the trust root ${pinned:-nothing}, but scripts/sigstore_trusted_root.json is $actual. Update them together, as docs/releasing.md says."
  echo "install.sh pins scripts/sigstore_trusted_root.json, $actual."
}

# offline runs cosign verify-blob in a network namespace of its own, with an
# empty TUF cache, so it can reach neither Sigstore nor anything else. Each run
# gets a fresh cache, because cosign runs as root there and leaves root-owned
# files behind.
offline() {
  sudo unshare --net -- env HOME="$work" TUF_ROOT="$(mktemp -d)" "$(command -v cosign)" verify-blob "$@"
}

check_sigstore() {
  local cosign_version cosign_releases tuf current tag
  cosign_version=$(sed -n 's/^COSIGN_VERSION=//p' scripts/install.sh)
  cosign_releases=$(sed -n 's/^COSIGN_RELEASES=//p' scripts/install.sh)
  work=$(mktemp -d)

  tuf=$(mktemp -d)
  TUF_ROOT="$tuf" cosign initialize >/dev/null
  current="$tuf/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json"
  [[ -f $current ]] ||
    fail "" "cosign initialize left no trusted_root.json in $current, so the pin can't be compared. Find where this cosign keeps it, and fix this check."
  cmp -s "$current" "$root" ||
    fail scripts/sigstore_trusted_root.json "Sigstore's trust root is now $(sha256sum <"$current" | cut -d ' ' -f 1), and installs through a mirror will fail once Sigstore signs with what changed. Update scripts/sigstore_trusted_root.json and the pin in install.sh, as docs/releasing.md says."
  note "scripts/sigstore_trusted_root.json is the trust root Sigstore publishes now."

  for file in cosign_checksums.txt cosign_checksums.txt.sigstore.json; do
    curl -fsSL --retry 3 -o "$work/$file" "$cosign_releases/download/$cosign_version/$file"
  done
  local cosign_release=(--bundle "$work/cosign_checksums.txt.sigstore.json"
    --certificate-identity keyless@projectsigstore.iam.gserviceaccount.com
    --certificate-oidc-issuer https://accounts.google.com "$work/cosign_checksums.txt")
  if offline "${cosign_release[@]}" 2>/dev/null; then
    fail "" "cosign verified through TUF from inside the network namespace, so the network wasn't cut off and nothing was checked offline. Fix how this script cuts cosign off."
  fi
  offline --trusted-root "$root" "${cosign_release[@]}"
  note "cosign $cosign_version, signed keyless on Sigstore's public instance, verifies offline against the pinned trust root."

  tag=$(scripts/latest-release.sh "$repo")
  if [[ -z $tag ]]; then
    note "$repo has no published release yet, so only cosign's own release was verified offline."
    return
  fi
  gh release download "$tag" -R "$repo" -D "$work/release" -p checksums.txt -p checksums.txt.sigstore.json
  offline --trusted-root "$root" --bundle "$work/release/checksums.txt.sigstore.json" \
    --certificate-identity "$server/$repo/.github/workflows/release.yml@refs/tags/$tag" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com "$work/release/checksums.txt"
  note "Dockwell $tag verifies offline against the pinned trust root, as an install through a mirror checks it."
}

case ${1:-} in
pin) check_pin ;;
sigstore)
  check_pin
  check_sigstore
  ;;
*)
  echo "Usage: scripts/check-trust-root.sh pin|sigstore" >&2
  exit 2
  ;;
esac
