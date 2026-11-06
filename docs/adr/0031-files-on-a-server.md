# 0031. Files on a server, for a person, outside revisions

- Status: Accepted
- Date: 2026-11-06

## Context

Issue #87 asks to browse, read, edit and save any file on any server from the panel, through the agent and the public API. It was about half of what was done on the panels of the two real servers looked at. Three earlier decisions bear on it:

- PRD principle 2 makes every change to managed state a revision with a plan.
- [ADR 0016](0016-scoped-api-tokens.md) gives tokens two scopes by a method's effect, and neither was made with a root agent's whole disk in mind.
- [ADR 0022](0022-secrets-encrypted-at-rest.md) draws its boundary at the API: it never returns a secret's value without a step-up. The agent runs as root, and the install's key, the database, and Docker's copy of every container's environment are all files.

## Decision

### A save isn't a revision

Managed state is what Dockwell holds the desired form of: it can plan a change to it, see it drift, and undo it. A file on a server is none of that. Anything on the server may write to it, Dockwell keeps no copy, and there is nothing to plan against.

So a save has no plan, no revision and no undo, and `FileService` doesn't reach `internal/revision`. What a save leaves on the control plane is one audit event, written with `store.DB.Update`, as a reveal's is. The rule that only `internal/revision` calls `store.DB.Revise` holds unchanged. The mock's "You can undo the save from Activity" comes out of the screen.

A save can't go around a plan, as what Dockwell manages stays out of reach: its own directories are refused below. A spec that mounts a file names its path, not what's in it, so editing such a file changes nothing a plan knows of.

### A person, never a token

Every method of `FileService` is `(dockwell.v1.needs_session)`, so no token reaches it, read-only or read-write. The handlers refuse a token's principal too, as `RevealSecret`'s does. `ListFiles` and `ReadFile` are `EFFECT_READ`. `SaveFile` is `EFFECT_DESTRUCTIVE`, as it can't be undone.

Read-only tokens are refused as well: reading the disk is worth more than the server list, as a read-only token's owner meant it. A grant of its own, such as the logs grant of [ADR 0025](0025-logs-per-deploy.md), is how a script would get this. Nothing asks for it yet, and it takes its own record.

### What's refused, for everyone

The agent refuses these whoever asks. No step-up opens them.

| Refused | Why |
| --- | --- |
| Dockwell's data directory | The install's key and the database, which together open every secret; the agent's key; the proxy's certificates |
| Docker's data root, other than each volume's `_data`, and containerd's | `containers/<id>/config.v2.json` holds a container's environment, secrets' values included, and a volume's `opts.json` a CIFS password |
| `/proc`, `/sys`, `/dev`, `/run` | These aren't files. `/proc/<pid>/environ` is a process's environment, `/proc/<pid>/root` a container's disk, `/dev/sda` the server's, and `/run/containerd` holds each container's runtime spec with its environment |
| `/etc/shadow`, `/etc/gshadow`, `.pgpass`, and any file whose text holds password hashes or a PEM private key | They can be told for certain, and have no reason to be in a browser. A backup of a shadow file goes by what it holds, as its name proves nothing. SSH host keys, users' SSH keys and TLS keys are all PEM |
| Anything that isn't a regular file or a directory | Sockets, FIFOs and devices have nothing to show, and reading a FIFO would hang |

A step-up doesn't open these because one tap would open every secret at once, where ADR 0022 asks one tap for each.

Not knowing where Docker's data is must never mean serving it. The engine says where it is whenever it answers, and the agent keeps each place it has said on disk, so a restart with Docker away, or a root that moved, still knows. A directory that holds both `containers` and `volumes` counts as a data root whoever says so. `config.v2.json`, `hostconfig.json`, a volume's `opts.json`, and text with Docker's own keys in it are refused wherever they turn up.

`.env`, `wp-config.php` and the like stay readable. Dockwell can't tell a config with a password in it from one without, and reading them beside a running app is what the screen is for. [The threat model](../security/files.md) says so.

### The agent decides, on what it opens

The control plane can't see the server, and a path's text proves nothing: an app can plant a symlink in its volume, pointing at the install's key, and swap it after a check. So the agent walks a path from `/` one part at a time, with `openat` and `O_NOFOLLOW`, follows links itself, and checks each directory's device and inode against the refused ones, which it reads again for each call. What it checked is the descriptor it reads. A symlink, a `..` in a link's target, or a bind mount of a refused directory leads nowhere new. A request's own path has no `.` or `..`, as in [ADR 0021](0021-the-docker-driver.md#host-access).

### No step-up

A step-up here would stop less than it seems to, and cost more.

- A session already controls every server. It can apply a plan that mounts `/etc` into a container, which takes a person's session and no passkey (ADR 0021), so a stolen cookie reaches what `FileService` serves anyway. The sign-in threat model says the same of stolen sessions.
- This is the most used screen. A passkey prompt on it, even one that lasts some minutes, teaches people to tap through the prompt that does guard something, `RevealSecret`. It would also shut Files to anyone on a tunnel or with codes and no passkey for that address.
- What a step-up guards today, a secret's value, is refused here outright, and the audit log keeps what a stolen session read and saved.

If plans with host access or the terminal come to take a step-up, this takes the same one, in the same record.

### On record

- `file.read` for each `ReadFile`, naming the person, the server and the path, written before the contents go out. If it can't be written, nothing goes out, as with `secret.reveal`.
- `file.refuse` for each refused read of a file, as someone probing for the key should show.
- `file.save` for each save, which is on the timeline too, as it changes the server.
- A file's contents are never recorded or logged. `ListFiles` isn't recorded: it returns names, and it runs on every click.

### Bounds

- A file reads as text up to 1 MiB. A larger one reads as its first 1 MiB, and can't be saved. One that isn't UTF-8 reads as binary, and its contents aren't sent.
- A directory lists at most 5,000 entries and 4 MiB.
- These calls go on the agent's main stream ([ADR 0017](0017-the-change-api-and-the-agent-stream.md)), at most 4 at once, as they are this small. Uploads and downloads, which aren't, would go on a connection of their own, as logs do.

## Consequences

- The file manager works on every server, as it goes through the agent, with nothing on the agent's disk and no new listener. It adds one package to the agent build, and no module.
- `.env` files and the like are readable with a session and no passkey, and a stolen session can read them until it is revoked. The audit log shows which.
- Saving has no undo, so the panel has to make a save safe another way: it notices a file that changed since it was opened, and keeps what you typed.
- When roles come, Files belongs to whoever may be root on that server.
- The refused list has to grow with what Dockwell and Docker write. A new place that holds a value belongs on it, in the same change.
