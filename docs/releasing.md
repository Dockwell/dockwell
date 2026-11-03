# Releasing Dockwell

Releases are cut from `main` by pushing a tag, and [`.github/workflows/release.yml`](../.github/workflows/release.yml) does the rest in two jobs:

- **Build** has read-only permissions and uses no caches. It refuses the tag unless it points at a commit on `main` whose required CI checks all passed. It then builds with GoReleaser ([`.goreleaser.yaml`](../.goreleaser.yaml)), smoke-tests the binary, and hands the artifacts on.
- **Publish** runs none of the build code. It signs, attests and publishes what Build produced.

Nobody builds release artifacts on a laptop, and no npm package or build script ever holds the signing or publishing credentials.

## What a release contains

For each platform (linux amd64 and arm64):

- `dockwell_<version>_linux_<arch>.tar.gz`: the static `dockwell` binary with the panel built in, plus `LICENSE` and `README.md`.
- `dockwell_<version>_linux_<arch>.tar.gz.spdx.json`: an SPDX software bill of materials for that archive.

And once per release:

- `checksums.txt`: SHA-256 of every archive.
- `checksums.txt.sigstore.json`: a keyless cosign signature of `checksums.txt`, made with the release workflow's GitHub identity and recorded in Sigstore's public transparency log.
- A GitHub build-provenance attestation for every archive, SBOM and the checksums, which says the workflow and commit that built them.

Builds are reproducible. The same commit gives byte-identical binaries, archives and `checksums.txt`, because paths are trimmed, file times and owners are fixed, and the build ID is empty. SBOMs embed their creation time, so they differ between builds; the provenance attestation covers them instead of the checksums.

Every push to `main` also runs the same build as a snapshot, without signing or publishing. The workflow run keeps the snapshot for 14 days as an artifact named `dockwell-snapshot`.

## Cut a release

Versions follow [semantic versioning](https://semver.org). Before 1.0, a minor bump may break compatibility, and its release notes say so.

1. Make sure the milestone for the release is closed or moved on. The workflow checks that the commit you tag is on `main` and green. It waits up to 20 minutes for CI that is still running.
2. Tag the commit and push the tag. Only admins can create `v*` tags, and a tag can't be moved or deleted once pushed:

   ```sh
   git tag -s v0.1.0 -m "Dockwell v0.1.0"
   git push origin v0.1.0
   ```

   A tag with a suffix, such as `v0.2.0-rc.1`, is published as a pre-release.
3. Watch the Release workflow. When it finishes, the GitHub release has the archives, the SBOMs, the checksums with their signature, and notes that GitHub generates from the pull requests merged since the last release. Edit the notes afterwards to call out anything that breaks compatibility.

To check the build locally first, run `make release-snapshot`. It writes the same archives to `dist/` that CI builds for `main`, with SBOMs too if [syft](https://github.com/anchore/syft) is installed. GoReleaser is pinned in `tools/go.mod`, so the local and CI builds use the same version.

## Verify a release

With the GitHub CLI, one command checks that an archive was built by this repository's release workflow:

```sh
gh attestation verify dockwell_0.1.0_linux_amd64.tar.gz -R Dockwell/dockwell
```

Without it, verify the signed checksums with [cosign](https://docs.sigstore.dev/cosign/system_config/installation/), then the archive against them:

```sh
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/Dockwell/dockwell/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing --check checksums.txt
```

The install script verifies downloads on your behalf (issue #13).
