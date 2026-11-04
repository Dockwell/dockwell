# 0010. The install script verifies releases with a pinned cosign

- Status: Accepted
- Date: 2026-11-04

## Context

`curl -fsSL https://get.dockwell.dev | sh` must check the release signature before it installs anything (#13), and get from start to a running panel in under 60 s on a 1 vCPU VPS (PRD: non-functional requirements).

Releases are signed keyless ([releasing](../releasing.md)). The release workflow signs `checksums.txt` with cosign under its own GitHub identity, Fulcio certifies the short-lived key, and Rekor logs the signature in public. There is no long-lived key to guard or lose, and every signature ever made is on the record.

Checking a keyless signature takes four checks: the certificate chains to Sigstore's roots, it names the right identity, the transparency log recorded the signature while the certificate was valid, and the signature matches. A fresh server has no tool that does all four. `sha256sum` is everywhere. `openssl` is on most Debian, Ubuntu and RHEL installs but not on Alpine. `ssh-keygen -Y verify` needs OpenSSH 8.1, which RHEL 8 lacks. `gpg` isn't on Alpine, and Debian 13's apt verifies with `sqv` instead. None of them reads a Sigstore bundle.

The options:

1. **Fetch a pinned cosign.** The script carries cosign's version and the SHA-256 of its linux amd64 and arm64 binaries. It downloads cosign from its GitHub release, checks the hash, and runs `cosign verify-blob` for the exact identity of the release workflow at that release's tag. That is full keyless verification with no new key. The cost is size: cosign v3.1.3 is 141 MB for amd64 and 133 MB for arm64, against 7.9 MB for Dockwell's own archive. On a well-connected machine, downloading and verifying took under 3 s. On a 100 Mbit/s link, the download alone takes about 12 s.
2. **Verify with a tool already on the server.** This works only with a long-lived key, such as an SSH signature checked with `ssh-keygen -Y verify`, or a raw signature checked with `openssl dgst`. Neither tool is on every supported system. Either way, a new private key would live as a repository secret, to guard, rotate and revoke. A leak could go unnoticed, where keyless signing leaves a public entry for every signature.
3. **Check the Sigstore bundle with openssl and shell.** Verify the chain to Fulcio's root with `openssl verify`, parse the identity out of the certificate's extensions, and check the signature and Rekor's inclusion promise. It can be done, but it is a hand-written Sigstore verifier in POSIX `sh`. It would be tied to a bundle format that changed in cosign v3 and to Sigstore's root rotations, and `openssl` isn't on Alpine.
4. **Publish a small verifier of Dockwell's own**, built from sigstore-go and pinned by hash as cosign would be. It would be perhaps a tenth of cosign's size, but it is new code and a new release artifact to build and pin, to save seconds.

`gh attestation verify` checks the build provenance that every release also carries, but it needs the GitHub CLI.

## Decision

- The install script pins cosign by version and by the SHA-256 of each binary, and the version is the one `release.yml` signs with. It downloads cosign into a temporary directory and checks the hash. Then it verifies `checksums.txt` against its bundle. The certificate must name `https://github.com/Dockwell/dockwell/.github/workflows/release.yml@refs/tags/v<version>`, issued by `https://token.actions.githubusercontent.com`. Last, the archive must match its checksum. Nothing outside the temporary directory changes until all of that passes, and the directory is deleted afterwards.
- A run that would change nothing downloads nothing: when the installed version is already the one asked for, there is nothing to verify because nothing is installed.
- Two settings change this, and both are off by default:
  - `DOCKWELL_COSIGN` uses a cosign you already have, but only if it matches the pinned hash. It is for air-gapped installs and for the tests.
  - `DOCKWELL_VERIFY_KEY` verifies against a cosign public key instead of the release workflow's identity, without the transparency log. It is for mirrors that re-sign releases with their own key, and for the tests. The script says so when it is in use.
- No new key or repository secret.
- Option 4 is deferred, not rejected. Revisit it when the agent gains its own Sigstore verification for self-updates, or if the nightly install time shows the download hurting.

## Consequences

- An install trusts the script, as any `curl | sh` does. The pinned hash makes the cosign it downloads exactly as trustworthy as the script itself, and the script changes only through review in this repository.
- Each install that changes the binary downloads about 140 MB more, and needs to reach github.com and `tuf-repo-cdn.sigstore.dev`, from which cosign fetches Sigstore's trust root. The nightly install-time job measures the whole install on one CPU against the 60 s budget.
- Changing `COSIGN_VERSION` in `release.yml` means changing the pin in `scripts/install.sh` too, with both hashes taken from cosign's own verified `cosign_checksums.txt`. CI fails when the two versions differ, or when a pinned hash doesn't match the cosign that `sigstore/cosign-installer` verified.
- The identity names the tag, so a release's checksums can't be passed off under another version. It doesn't stop a downgrade: if the download host served an older, genuinely signed release as the latest, the script would install it. Pin a version with `--version` if that matters to you.
- When the agent updates itself (PRD: "self-updates from signed releases"), it verifies with its own code and needs no cosign. The same code could then become the small verifier of option 4.
