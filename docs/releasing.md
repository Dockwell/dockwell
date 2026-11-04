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

- `checksums.txt`: SHA-256 of every archive, `install.sh` and `sigstore_trusted_root.json`.
- `checksums.txt.sigstore.json`: a keyless cosign signature of `checksums.txt`, made with the release workflow's GitHub identity and recorded in Sigstore's public transparency log.
- `install.sh`: the install script at that release's tag.
- `sigstore_trusted_root.json`: the copy of Sigstore's trust root that this `install.sh` pins, for installs through a [mirror](install.md#install-through-a-mirror) that check signatures without reaching Sigstore ([ADR 0013](adr/0013-mirrors-for-releases-and-images.md)).
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

Where you can't reach Sigstore, add `--trusted-root sigstore_trusted_root.json` to the cosign command, and cosign checks against that file instead of fetching Sigstore's trust root. First check that the file's SHA-256 is the `SIGSTORE_ROOT_SHA256` in `install.sh`, from a copy of the script you trust.

The [install script](install.md) runs the same checks before it installs anything, with a cosign it downloads and checks against a pinned SHA-256 ([ADR 0010](adr/0010-install-script-verifies-with-pinned-cosign.md)).

## Update cosign

`COSIGN_VERSION` in the release workflow and the pin in [`scripts/install.sh`](../scripts/install.sh) move together, so the install script always verifies with the cosign that signs. To change them:

1. Download `cosign_checksums.txt` and its `.sigstore.json` bundle from the new cosign release, and verify them with the cosign you have:

   ```sh
   cosign verify-blob cosign_checksums.txt --bundle cosign_checksums.txt.sigstore.json \
     --certificate-identity keyless@projectsigstore.iam.gserviceaccount.com \
     --certificate-oidc-issuer https://accounts.google.com
   ```

2. Set `COSIGN_VERSION` in `release.yml` and in `install.sh`, and copy the SHA-256s of `cosign-linux-amd64` and `cosign-linux-arm64` from the checksums into `install.sh`.
3. Change the cosign version in the mirror section of [installing](install.md#install-through-a-mirror), which says where mirrors keep cosign.

The Install workflow fails when the two versions differ, or when a pinned hash doesn't match the cosign that `sigstore/cosign-installer` verified.

## Update Sigstore's trust root

Installs through a mirror check signatures against `scripts/sigstore_trusted_root.json`, a copy of Sigstore's trust root that `install.sh` pins as `SIGSTORE_ROOT_SHA256` ([ADR 0013](adr/0013-mirrors-for-releases-and-images.md)). Sigstore changes the trust root when it adds or retires a key. The Sigstore trust root workflow then fails: nightly, when it opens an issue or comments on the open one, and on pull requests that change the install script or the pin. The release workflow runs the same check before it publishes, and refuses to publish a release whose signature doesn't verify against the copy. To update it:

1. Fetch the current trust root with cosign's TUF client, which checks it against the TUF root built into cosign:

   ```sh
   export TUF_ROOT=$(mktemp -d)
   cosign initialize
   cp "$TUF_ROOT/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json" scripts/sigstore_trusted_root.json
   sha256sum scripts/sigstore_trusted_root.json
   ```

2. Set `SIGSTORE_ROOT_SHA256` in `install.sh` to that SHA-256.
3. Read the diff. A new key or certificate authority comes with a `validFor.start`, and a retired one gains an `end`.
4. Merge it, then cut a release. Each release's script pins the trust root that release carries, and a script from a mirror's latest release uses that one, so mirrors pick up the change with the next release.
