#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Runs the install tests (run.sh) in stock distribution containers, against
# the test releases that releases.sh lays out:
#
#   scripts/install-test/test.sh <releases> <cosign> <image>...
#
# The releases are served over HTTP on the host's loopback, as GitHub serves
# them, and each container shares the host's network to reach them. <cosign>
# is a copy of the cosign that install.sh pins, for this machine's
# architecture; the first install in each container downloads its own, and
# the rest reuse this one. Needs Docker and python3.
set -euo pipefail

releases=$(realpath "${1:?usage: test.sh <releases> <cosign> <image>...}")
cosign=$(realpath "${2:?usage: test.sh <releases> <cosign> <image>...}")
shift 2
(($#)) || {
  echo "test.sh: name at least one image, such as debian:13." >&2
  exit 2
}
repo=$(git rev-parse --show-toplevel)
# shellcheck source=/dev/null # written by releases.sh
source "$releases/versions"
port=${RELEASES_PORT:-8099}

case $(uname -m) in
x86_64) pin=$(sed -n 's/^COSIGN_SHA256_AMD64=//p' "$repo/scripts/install.sh") ;;
aarch64) pin=$(sed -n 's/^COSIGN_SHA256_ARM64=//p' "$repo/scripts/install.sh") ;;
*) pin= ;;
esac
cosign_env=(-e COSIGN=/cosign)
if [[ "$(sha256sum "$cosign" | cut -d ' ' -f 1)" != "$pin" ]]; then
  echo "test.sh: $cosign isn't the cosign install.sh pins, so every install downloads its own." >&2
  cosign_env=()
fi

python3 -m http.server --bind 127.0.0.1 --directory "$releases" "$port" >/dev/null 2>&1 &
server=$!
trap 'kill "$server" 2>/dev/null || true' EXIT
for _ in $(seq 50); do
  curl -fsS -o /dev/null "http://127.0.0.1:$port/versions" 2>/dev/null && break
  sleep 0.1
done

for image in "$@"; do
  [[ -z "${GITHUB_ACTIONS:-}" ]] || echo "::group::$image"
  docker run --rm --network host \
    -v "$repo/scripts:/scripts:ro" \
    -v "$releases:/releases:ro" \
    -v "$cosign:/cosign:ro" \
    -e INSTALL=/scripts/install.sh \
    -e RELEASES="http://127.0.0.1:$port" \
    -e KEY=/releases/cosign.pub \
    -e OLD="$OLD" -e NEW="$NEW" \
    "${cosign_env[@]}" \
    "$image" sh -c 'sh /scripts/install-test/prepare.sh && sh /scripts/install-test/run.sh'
  [[ -z "${GITHUB_ACTIONS:-}" ]] || echo "::endgroup::"
done
