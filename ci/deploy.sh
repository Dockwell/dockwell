#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only

set -euo pipefail
shopt -s inherit_errexit

fail() { printf '%s\n' "$*" >&2; exit 1; }

: "${DOCKWELL_URL:?Set the Dockwell panel address}"
: "${DOCKWELL_TOKEN:?Set the DOCKWELL_TOKEN secret}"
: "${DOCKWELL_SERVICE:?Set the Dockwell service ID}"
: "${DOCKWELL_IMAGE:?Set the image built by CI}"
: "${DOCKWELL_VERSION:?Set the Dockwell release tag}"

[[ $DOCKWELL_VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail "Use a Dockwell release tag, such as v0.4.0."
[[ $(uname -s) == Linux ]] || fail "Use a Linux runner."
doc_url=$DOCKWELL_URL
case "$doc_url" in
  https://* | http://127.0.0.1:* | http://localhost:*) ;;
  *) fail "Use the panel's HTTPS address." ;;
esac
case "${DOCKWELL_CI_PROVIDER:-}" in
  github-actions)
    repository="${GITHUB_SERVER_URL:?}/${GITHUB_REPOSITORY:?}"
    commit=${GITHUB_SHA:?}
    ref=${GITHUB_REF:?}
    run_url="$repository/actions/runs/${GITHUB_RUN_ID:?}"
    provider=DEPLOY_PROVIDER_GITHUB_ACTIONS
    ;;
  gitlab-ci)
    repository=${CI_PROJECT_URL:?}
    commit=${CI_COMMIT_SHA:?}
    ref=${CI_COMMIT_REF_NAME:?}
    run_url=${CI_PIPELINE_URL:?}
    provider=DEPLOY_PROVIDER_GITLAB_CI
    ;;
  *) fail "Choose GitHub Actions or GitLab CI." ;;
esac
origin=$(jq -nc --arg repository "$repository" --arg commit "$commit" --arg ref "$ref" --arg runUrl "$run_url" \
  --arg provider "$provider" '$ARGS.named')

# Keep these pins aligned with scripts/install.sh. Verify the verifier before
# using it, then verify the release workflow identity and the archive's digest.
case $(uname -m) in
  x86_64)
    arch=amd64
    cosign_sha=4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71
    ;;
  aarch64 | arm64)
    arch=arm64
    cosign_sha=c5d324e091826b0d7a78eb16fef316450b4eb9aaec045611c08ba06f5e73220a
    ;;
  *) fail "Use an amd64 or arm64 runner." ;;
esac
cosign_version=v3.1.3
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
fetch() { curl -q --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 300 --retry 3 --retry-max-time 360 -o "$2" "$1"; }
repo=https://github.com/Dockwell/dockwell
release="$repo/releases/download/$DOCKWELL_VERSION"
archive="dockwell_${DOCKWELL_VERSION#v}_linux_$arch.tar.gz"
fetch "https://github.com/sigstore/cosign/releases/download/$cosign_version/cosign-linux-$arch" "$work/cosign"
printf '%s  %s\n' "$cosign_sha" "$work/cosign" | sha256sum --check --status || fail "Cosign's checksum doesn't match."
chmod +x "$work/cosign"
fetch "$release/checksums.txt" "$work/checksums.txt"
fetch "$release/checksums.txt.sigstore.json" "$work/checksums.txt.sigstore.json"
TUF_ROOT="$work/tuf" "$work/cosign" verify-blob \
  --certificate-identity "$repo/.github/workflows/release.yml@refs/tags/$DOCKWELL_VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle "$work/checksums.txt.sigstore.json" "$work/checksums.txt"
archive_sha=$(awk -v file="$archive" '$2 == file { print $1 }' "$work/checksums.txt")
[[ $archive_sha =~ ^[0-9a-f]{64}$ ]] || fail "The signed checksums don't list $archive."
fetch "$release/$archive" "$work/$archive"
printf '%s  %s\n' "$archive_sha" "$work/$archive" | sha256sum --check --status || fail "The release archive's checksum doesn't match."
tar -xzf "$work/$archive" -C "$work" dockwell

"$work/dockwell" services deploy --wait --image="$DOCKWELL_IMAGE" --deploy-origin="$origin" -- "$DOCKWELL_SERVICE"
