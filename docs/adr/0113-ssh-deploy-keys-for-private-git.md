# 0113. Private Git over SSH deploy keys

- Status: Accepted. Extends [ADR 0046](0046-builds-from-git.md) and [ADR 0098](0098-connecting-github-as-an-app.md).
- Date: 2026-11-27

## Context

Dockwell builds private repositories only through a connected GitHub App ([ADR 0098](0098-connecting-github-as-an-app.md)). A private repository on GitLab, Gitea, Forgejo, Bitbucket or a self-hosted Git server cannot be built unless it is made public.

An SSH deploy key gives read-only access to one repository on any Git host that speaks SSH, without handing Dockwell a personal access token that reaches every repository in an account.

What already exists to build on:

- Every managed resource with a `(sensitive)` field is envelope-encrypted at rest in the revision store, masked in plans, and fingerprinted with a keyed HMAC ([ADR 0022](0022-secrets-encrypted-at-rest.md)).
- `internal/netguard` guards outbound connections against metadata services, loopback, link-local, multicast, broadcast and reserved ranges after DNS resolution.
- `internal/gitfetch` verifies Git packs with SHA-1DC collision resistance and bounded object count, delta depth and inflation budgets.
- `ImageBuild.git_login` (field 101) already threads repository credentials through plans, builds, `DetectBuild`, `ListRepositoryBranches`, push-to-deploy hooks and update checks.
- The agent's `ReceiveFile` stream already carries binary data from the control plane to an agent on a flow-controlled connection of its own.
- Dockwell's SFTP backup work (ADR 0112) established an SSH host-key pinning model: negotiate an explicit `HostKeyAlgorithms` list preferring Ed25519, ECDSA, then RSA with SHA-2, compare the OpenSSH SHA-256 fingerprint in constant time, abort before authentication when unverified or changed, and probe unauthenticated to report a changed host key.

## Decision

### Resource model: `git.deploy_key`

- **A dedicated `git.deploy_key` kind** (`gdk_…`, `LevelProject`, `Spec.git_deploy_key = 135`), rather than a generic Git credential kind. A deploy key is project-scoped, pins one SSH host key, and holds a pending replacement key during rotation until a test succeeds. GitHub Apps (`github.app`) are organization-scoped and future provider connections carry account tokens and API endpoints rather than SSH host keys.
- **Sealed at rest, never returned.** Both `private_key` and `pending_private_key` carry `(sensitive) = true` and `(no_flag) = true`, so the revision store seals every revision and change item under [ADR 0022](0022-secrets-encrypted-at-rest.md), rotates on change without equality comparison (`resource.Rotates`), and never leaks either key through any API response, CLI output, plan diff, log, Activity entry or error. Because revision history is immutable under ADR 0022, prior sealed revisions of a rotated or deleted key stay envelope-encrypted in `revisions.spec` and `change_items.spec` (never returned by any API and never re-activated because `UndoChange` is refused), just as other rotated or deleted secrets do; removing the old public key from the Git host revokes its access immediately.
- **Public metadata.** API reads (`GitDeployKeyInfo`) show the key's name, project, normalized repository URL, host, port, user, key type (`ssh-ed25519`), OpenSSH public key, key SHA-256 fingerprint, pinned host key type and SHA-256 fingerprint, created and last-rotated timestamps, pending rotation public key (when rotating), recognized deploy-keys settings link, and services using it.
- **Project isolation.** A deploy key belongs to one project. Plans, branch listings, detections, push-to-deploy reads and builds refuse any key that does not belong to the app's project or does not match the build's repository.
- **No direct ChangeService mutation, apply, discard or automatic undo.** `git.deploy_key` is listed in `changes.ownMethods` and `changes.ownApply`, so external `ChangeService.ProposeChange`, `ApplyChange` and `DiscardChange` calls cannot create, re-pin, rotate, apply, discard or delete a deploy key directly. All mutations go through `GitDeployKeyService` (which enforces role checks, passkey step-up, host-key verification and in-use checks) via the internal `Changes.Propose` + `ApplyOwned` path, and `applyEdit` calls `DiscardOwned` if `ApplyOwned` fails to start so no planned key change is left behind. `ActsElsewhere` refuses automatic undo for every `git.deploy_key` change.

### Naming on a service and repository URLs

- **`ImageBuild.git_login` takes `"ssh:<key-name>"`** alongside GitHub App logins (`"<App slug>/<installation ID>"`). Because `:` is never allowed in a GitHub App slug, the two forms cannot collide. Plans and the Source panel identify deploy-key builds by key name (`with deploy key <name>`).
- **SSH repository addresses.** One shared parser in `internal/resource/service` (re-exported by `internal/gitssh`) accepts `ssh://[user@]host[:port]/path` (preserving absolute paths like `/srv/git/app.git` for plain `sshd`/`git-shell` servers and `/~/` relative paths) and SCP-style `user@host:path` (preserving relative `user@host:owner/repo.git` vs absolute `user@host:/srv/git/app.git`). `SameSSHRepository` keeps absolute `/path` distinct from relative `path` (stripping leading `/` only for display and webhook matching). Leading-hyphen repository paths, passwords in SSH URLs and cleartext `git://` are refused.
- **HTTPS fallback in the UI.** Pasting an `ssh://` or `git@host:path` address, or an HTTPS URL that answers 401/404, offers "Use a deploy key" in place.
- **Standalone Docker image builds.** `ResolveDockerGit` refuses SSH repository URLs and `ssh:` deploy-key logins at review time in slice 1 (stating that standalone Docker image builds fetch HTTPS repositories and pointing to app builds); SSH deploy keys for standalone Docker image builds remain for slice 2.

### Control-plane pure-Go Git-over-SSH (`internal/gitssh`)

- **SSH Git runs on the control plane only.** The control plane generates Ed25519 keys in memory, dials Git hosts in pure Go with `golang.org/x/crypto/ssh`, runs `git-upload-pack '<repo>'` over a single `"session"` channel, speaks Git smart protocol v2 with protocol v1 fallback, and verifies received packs with `internal/gitfetch`'s SHA-1DC verifier.
- **Private keys stay off disk and off agents.** A private key is decrypted in control-plane memory only for the duration of an SSH dial and cleared immediately after building the `ssh.Signer`. No private key is ever written to disk, sent over the agent stream, mounted into a container or placed in an environment variable or build context. Temporary verified packs are staged in a Dockwell-owned directory (`<DataDir>/ssh-packs`, mode `0700`) swept on control-plane startup, and each pack file is closed and removed as soon as the agent finishes reading it over `ReceiveFile` or when the build/detect call exits, whichever comes first.
- **Zero agent footprint.** For `DetectBuild` and `Build` steps that use a deploy key, the control plane fetches and verifies the shallow pack (confirming that the requested commit is on the requested branch or tag) and streams the verified pack bytes to the building agent over the existing `ReceiveFile` lane (`DesiredResource.build_git_bundle_stream = 366`, `DetectBuildCall.git_bundle_stream = 367`, gated by `Hello.build_git_bundle = 365`). The agent mounts the pack read-only inside the build or detect container, which loads it with `git index-pack --stdin --keep`. The `-tags agent` build links no new packages or modules.
- **Network, transport, concurrency and size bounds.**
  - Dials through `netguard.Dialer(netguard.Options{Private: true})`, matching HTTPS Git's private-network rules (allowing RFC 1918, ULA and CGNAT for self-hosted Git servers while blocking loopback, link-local, cloud metadata, multicast, broadcast, unspecified, reserved ranges and NAT64).
  - Enforces explicit timeouts on dial (10 s), SSH handshake (10 s), ref listing (10 s), and pack fetch (2 min total, 30 s idle).
  - Bounds kept ref listing bytes to 2 MiB (streaming line-by-line with 4 KiB per-pkt-line cap and 10,000 kept branches/tags max; protocol v0/v1 streams and drops non-branch/tag refs such as `refs/pull/*` up to a 32 MiB wire cap in O(1) memory) and enforces a fair shared control-plane SSH concurrency gate (4 total, 2 listings with 1 per project, 2 pack fetches with 1 per project, 1 listing per key). Interactive listings wait up to 2 s in FIFO order before returning `ResourceExhausted` (`too_many_readings`), while build and detection pack fetches wait in FIFO order bounded by their own step context timeout so another project's build cannot starve them.
  - Enforces `gitfetch`'s pack size limit (64 MiB default), object count (100,000), delta depth (64), indexing inflation budget (4x pack limit) and shallow depth bounds (depth 1 at tip, depth 200 when verifying an older commit on a branch). Because `VerifyPack` only reads commit objects and their parent pointers, it indexes only commit names in `byName` (while still SHA-1DC verifying every object in the pack) and caps commit objects at 1 MiB, delta cache at 4 MiB, and commit delta materialization at 64 MiB so two concurrent hostile packs stay well inside the 250 MB control-plane RSS budget.
  - Opens no PTY, enables no SSH agent forwarding, accepts no port forwarding and rejects all server-initiated channel types.
- **Host-key pinning.** Follows the same pinning logic as ADR 0112: explicit `HostKeyAlgorithms` locked to the pinned key type, constant-time `SHA256:...` check, handshake aborted before client authentication on an unverified or changed key, and an unauthenticated probe on algorithm mismatch to report the observed key type and fingerprint. A changed host key observed during any user or background fetch is recorded on the deploy key (via `changes.AsDockwell` when no user principal is present) and stops all fetches until an admin verifies and accepts the new fingerprint; a subsequent passing `TestGitDeployKey` without admin acceptance never clears the "Host key changed" warning. Pre-recognizing well-known hosts' published fingerprints (GitHub, GitLab, Bitbucket, Codeberg) to shorten first-pin onboarding remains for slice 2.

### Rotation, roles, step-up and audit

- **Safe rotation.** Starting a rotation generates a new Ed25519 key in `pending_*` while keeping the active key working. Testing with the pending key switches it into the active slot only after the test passes, removes the old private key from the active spec, and returns the old key's fingerprint with a link to the repository's deploy-keys page so the user can delete the old key on the host.
- **Roles and step-up.** A developer in the project may create, test, accept an initial host key, rotate and delete deploy keys in that project. Creating, rotating and deleting require a signed-in session and passkey step-up (`270`, `271`, `272`), matching API token creation. Accepting a **changed** host key on an already-pinned deploy key requires an admin or owner in the project and passkey step-up (`273`). All lifecycle actions are recorded in the audit log without private key material.

## Consequences

- Private repositories on GitHub, GitLab, Gitea, Forgejo, Bitbucket and self-hosted SSH Git servers build and deploy with read-only deploy keys.
- Deploy keys never leave the control plane and add zero packages or idle memory to the standalone agent build.
- Provider repository pickers and provider-managed webhooks for non-GitHub hosts remain for later slices.
