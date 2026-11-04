# 0013. Configurable mirrors for releases, images and the install script

- Status: Accepted
- Date: 2026-11-04

## Context

The PRD asks how Dockwell serves users in mainland China, with mirrors, registries and install scripts, from day one (#18). From there, GitHub's download hosts can be slow or cut off, and so can Sigstore's services. An install reaches both today ([ADR 0010](0010-install-script-verifies-with-pinned-cosign.md)):

- **github.com**, for the install script, the release archives, `checksums.txt` and its signature, and cosign, which is 141 MB;
- **tuf-repo-cdn.sigstore.dev**, from which cosign fetches Sigstore's trust root through TUF. The trust root holds the keys and certificates that a keyless signature is checked against: Fulcio's certificate authorities, Rekor's log keys, the certificate transparency logs, and the timestamp authority.

Later come container images, from Docker Hub, GHCR and other registries, once the Docker driver lands in M3, and agent self-updates.

Dockwell runs no infrastructure in China. Mirrors are run by others, such as a company with its own artifact server, an object storage bucket, or a service that proxies GitHub. Dockwell can't vouch for any of them, so the install has to treat every mirror as untrusted and check what it serves exactly as it checks a download from GitHub.

How a mirror is chosen:

1. **Detect it**, by location, time zone or how fast GitHub answers. Rejected: it surprises people, it can't be checked, and picking a mirror for someone vouches for it.
2. **A list of mirrors built into the script.** Rejected for the same reason, and the list would go stale.
3. **Name it explicitly, by URL.** Chosen.

How to check a keyless signature without reaching Sigstore. These were tested with the pinned cosign v3.1.3:

- **`cosign verify-blob --trusted-root <file>`** checks a bundle against that trust root and makes no network request. cosign's own help points there: `--offline` is deprecated, and its notice says "To verify in an airgapped environment, provide a --bundle with the signature and verification material, and a --trusted-root file with the service keys and certificates". The bundle carries the Rekor inclusion proof and the timestamp authority's signed timestamp, so nothing else is needed.
  - The test: cosign's own `cosign_checksums.txt` for v3.1.3, signed keyless on Sigstore's public instance, with every proxy variable pointed at a closed port and an empty `TUF_ROOT`.
  - Without `--trusted-root`, cosign failed fetching `https://tuf-repo-cdn.sigstore.dev/15.root.json`. With it, cosign said "Verified OK" and wrote nothing to `TUF_ROOT`.
  - It still refused the wrong `--certificate-identity`, and a changed blob.
  - CI repeats the check with cosign in a network namespace of its own.
- **Mirror Sigstore's TUF repository** and point cosign at it with `cosign initialize --mirror`. TUF would carry each change to the trust root on its own, but its timestamp metadata is short-lived: the copy fetched while writing this expired six days later. A mirror that falls that far behind would stop every install. It would also need a writable TUF cache and another setting.
- **Pin the trust root.** It is one JSON file of 6,787 bytes. It changes when Sigstore adds or retires a key, which happened three times in 2025: in January, in July to add the timestamp authority, and in September to add the first Rekor v2 log. It hasn't changed since. A pinned copy is as trustworthy as the script that pins it, like the cosign pin.

## Decision

### What a mirror is

- A mirror of Dockwell is a base URL that serves Dockwell's releases in GitHub's layout: `<mirror>/latest/download/<file>` and `<mirror>/download/v<version>/<file>`. `https://github.com/Dockwell/dockwell/releases` is such a URL, so anything that copies or proxies it with the same paths is a mirror.
- cosign has its own releases, in the same layout on its own repository. A second URL, which is optional, names a mirror of them: `<cosign mirror>/download/v3.1.3/cosign-linux-<arch>`. It is a separate setting because a proxy of GitHub serves each repository's releases under its own URL. `DOCKWELL_COSIGN` still uses a cosign that is already on the server.
- The install script takes them as `--mirror <url>` or `DOCKWELL_MIRROR`, and `--cosign-mirror <url>` or `DOCKWELL_COSIGN_MIRROR`.

### Where each file comes from

| What | Without a mirror | With `--mirror` |
| --- | --- | --- |
| Archives, `checksums.txt` and `checksums.txt.sigstore.json` | GitHub | The mirror |
| The install script | `get.dockwell.dev` or GitHub | The mirror: every release carries `install.sh` |
| cosign | GitHub | GitHub, or the `--cosign-mirror` |
| Sigstore's trust root | TUF, from `tuf-repo-cdn.sigstore.dev` | The mirror: every release carries `sigstore_trusted_root.json`, pinned by SHA-256 in the script |
| Container images, from M3 | Each image's registry | Registry mirrors, below |
| Agent self-updates, later | GitHub | The server's mirror setting, below |

Without a mirror, the script does exactly what it did before. Someone who can reach GitHub but not Sigstore can name GitHub itself as the mirror.

### Why any mirror can be trusted

Nothing from a mirror is trusted because of where it came from. Each file is checked as a download from GitHub is:

- cosign must match the SHA-256 pinned in the script.
- The trust root must match the SHA-256 pinned in the script, and cosign checks the signature against it: `cosign verify-blob --trusted-root`.
- `checksums.txt` must carry a signature from `https://github.com/Dockwell/dockwell/.github/workflows/release.yml@refs/tags/v<version>`, issued by `https://token.actions.githubusercontent.com`, as before.
- The archive must match its checksum.

A mirror can refuse to serve files, serve stale ones, or serve an older, genuinely signed release as the latest, the downgrade ADR 0010 already notes. It can't get the script to install anything Dockwell's release workflow didn't sign, and `--version` rules out the downgrade.

The trust root is the one difference from a download from GitHub. Through TUF, cosign gets each change Sigstore makes as soon as Sigstore makes it, including a key it retires early. A pinned copy gets that change once the pin moves. The pin is used only with a mirror, so the default path keeps TUF, and CI looks for a change every day (below).

### The pinned trust root

- `scripts/sigstore_trusted_root.json` is Sigstore's `trusted_root.json`, byte for byte as Sigstore's TUF repository serves it. `install.sh` pins its SHA-256 as `SIGSTORE_ROOT_SHA256`. `.gitattributes` and biome leave the file's bytes alone.
- Every release carries it as `sigstore_trusted_root.json`, with `install.sh`, and both are listed in the signed `checksums.txt`. The release workflow:
  - refuses to build when the file doesn't match the pin;
  - after signing, refuses to publish a release whose signature doesn't verify against the trust root that release carries.
- The script looks for the trust root in the release it installs, then in the latest release. The script attached to a release pins the trust root attached to that release. So the script from a mirror's latest release always finds its trust root there, and can install any older release with it, because old keys keep their dates in a newer trust root.
- The Install workflow's Sigstore trust root job runs on every pull request, every push to main, and nightly. It fails when the file isn't the trust root that cosign fetches through TUF now. Releases signed before a change keep verifying against the old pin. A release signed after it may need the new one, and the release workflow refuses to publish that release until the pin moves. [Releasing](../releasing.md#update-sigstores-trust-root) has the steps.

### How a server picks a mirror

- Whoever runs the install names it. The script never picks a mirror, whether by location, from a list, or by trying GitHub first.
- The script doesn't remember the mirror. Pass it again to upgrade.
- When agents update themselves, the mirror becomes a setting on the control plane, for the fleet or for one server. It is changed through a revision like any other managed state (PRD principle 2). The install script passes the mirror it used on to that setting.
- The agent downloads updates from the mirror. It verifies them with its own Sigstore code, as ADR 0010 plans, against a trust root built into the binary, which each release brings up to date. The checks are the same as the install script's, so an update through a mirror is as safe as an install.

### Container images, from M3

- Images are addressed by digest. The Docker driver will resolve each image to a digest when it plans a revision, then pull by that digest. Whatever registry or mirror serves the image, the digest proves its content, so a registry mirror needs no trust either.
- Docker's `registry-mirrors` setting covers Docker Hub only. The driver will take a map from registry to mirror, such as `ghcr.io` to `registry.example.com/ghcr`, as a revisioned setting for the fleet or a server. It then pulls `<mirror>/<path>@<digest>`.
- Until then, Docker pulls the way each server's own Docker is set up. [Installing](../install.md#container-images) says how to set `registry-mirrors` for Docker Hub.

### The install script itself

- Every release carries `install.sh`, listed in its signed checksums. A mirror serves it at `<mirror>/latest/download/install.sh`.
- When `get.dockwell.dev` is set up, it serves the latest release's `install.sh`, as a mirror does, so a script and the trust root it finds always match.
- A script piped from a mirror is trusted as `curl | sh` trusts `get.dockwell.dev`. To check it first, verify `checksums.txt` and compare the script's SHA-256 with it, as [releasing](../releasing.md#verify-a-release) describes.

## Consequences

- People in mainland China, and on any network that can't reach GitHub or Sigstore, can install Dockwell from a mirror. Every file gets the same checks, and nothing contacts Sigstore.
- Installs without a mirror don't change.
- There is a second pin to keep, and CI says when it is stale. Updating it means a pull request and a release, because the script from a mirror's latest release uses the trust root that release carries. Until that release, a script from main and an older mirror don't match, and the script says to use the mirror's own `install.sh`.
- Releases carry two more files: `install.sh` and `sigstore_trusted_root.json`.
- The install tests run the whole scenario through a mirror on a second URL, with GitHub and Sigstore cut off. A changed cosign, trust root, archive or signature on the mirror is refused. They also check that a keyless signature from Sigstore's public instance verifies against the pinned trust root offline. The test releases are signed with a key, so a keyless install of Dockwell itself through a mirror is first checked by the release workflow, on the first release.
- The docs name no mirror service, because Dockwell can't vouch for one. They say what a mirror must serve, and how to run your own.
