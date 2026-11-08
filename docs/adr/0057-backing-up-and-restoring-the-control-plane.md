# 0057. Backing up the control plane, and restoring it

- Status: Proposed
- Date: 2026-11-08

## Context

The PRD's failure and recovery: losing the control server takes the panel and API offline, workloads keep running, and the control plane's state is backed up off-site automatically and copied, encrypted, to two other agents. `dockwell control restore` promotes any agent, and with no backup, state is rebuilt from what the agents report. Issue #34 asks for it, done when backups are taken on every change batch and at least hourly, a restore works from an agent's copy or an off-site one, state can be rebuilt with no backup at all, and the procedure is documented and tested in CI.

What there is to build on:

- **Two things hold the whole control plane.** The store (SQLite at `<data>/control/state.db`, or Postgres given `--database`) holds every resource and revision, changes, people, passkeys, tokens, settings and the audit log, and, sealed under the install's key, the key-encryption keys, the data keys, every sealed spec, the CA's key and the authenticator secrets ([ADR 0022](0022-secrets-encrypted-at-rest.md)). The install's key, `<data>/control/key`, opens all of it. The panel's ACME account under `<data>/control/acme` is made again on its own, and the local token is the CLI's on that server.
- **Backups and their destinations exist** ([ADR 0033](0033-backups-of-volumes-and-databases.md)): a `backup.destination` is S3-compatible storage with a passphrase, a backup is sealed before it leaves under a key from the passphrase (PBKDF2, then AES-256-GCM in chunks bound to what the file is), and `dockwell open-backup` opens one with the passphrase alone, in either build.
- **Agents keep what their servers run** (ADR 0054, in #312): each agent holds a sealed record of each resource its server runs, with its values, its revision and its image, and a fill leaves alone what a restored store doesn't know. That is what a restore reconciles with, and what a rebuild starts from.
- **Agents dial out, and pin the CA.** An agent connects to the control URL its join ticket gave it, and trusts what the CA signs. The CA is in the store, so a restored control plane is the same CA to every agent; only its address can change.
- **A restore loses what the store learned since its backup.** The issue's own requirement (#120): a restored control plane doesn't know what it gave a server after the backup, so it can't tell which of an older agent's reads carry a value.

## Decision

### What is backed up: one sealed bundle

- **A bundle is the store, as a snapshot, and the install's key,** with what it was taken from (the build, the store's schema version, the install's ID, the store's generation, below) and when. Nothing else is needed to run the control plane again, and nothing outside the store and the key is in it.
- **The snapshot is a SQLite database at the store's schema version, whatever the store runs on.** On SQLite, `VACUUM INTO` makes it from one read transaction. On Postgres, the store copies every table into a fresh SQLite file within one read transaction. So a bundle restores into either, and a restore can move an install between them.
- **It's sealed whole with the control plane's recovery passphrase,** in ADR 0033's format, AES-256-GCM in 1 MiB chunks bound to the install's ID and the bundle's ID, with a stronger key:
  - **argon2id, 64 MiB, 3 passes, one lane:** RFC 9106's second recommended set, with one lane as the smallest server Dockwell supports has one CPU. It takes about a second on a 1 vCPU VPS with 1 GB, which slice 1 checks on one, raising the passes if it takes much less. The parameters and the salt are in each bundle's header, so a later bundle can ask for more.
  - **Derived once, not for each bundle:** a key from the passphrase and its salt, kept in memory, and each bundle's own key from that and the bundle's ID by HKDF-SHA-256. So the 64 MiB is spent once as the control plane starts, or as the passphrase is set, not every 10 minutes.
  - **Only the full build seals or opens one.** argon2id is in `golang.org/x/crypto`, which the full build links already and the agent build doesn't, and an agent never opens a bundle. `dockwell open-backup` and `dockwell control restore` in the full build open one; the agent build's `open-backup` says to use the full build.
- **The recovery passphrase** is made at the first start: 32 characters of Crockford's base32, 160 random bits, in groups of four. That's long enough that what stands between a stolen bundle and the install is the passphrase's own length, not the key derivation's cost. It's kept in the store sealed under the install's key, so the control plane can seal on its own. Settings → Backups shows it to the owner with a passkey just now, says to keep it somewhere that isn't this install, and the overview says so until the owner says it's kept. The owner can set one of their own, of at least 20 characters, which takes a passkey and is on Activity, and the panel says the one it made is stronger. Bundles sealed before keep the one they were sealed with, and the panel says so.
- **What that means.** A bundle and its passphrase are the whole install: its CA can sign a certificate any agent trusts, and every secret is in it. That is no more than the control server's own disk holds, and the panel says to treat the passphrase as such.

### Where it goes

- **To two agents.** The control plane sends each bundle to two connected agents other than its own server's, choosing those with the most room, and the same two while they stay connected. Each keeps it, sealed, in `<data>/agent/control-backups/`. An agent can't open what it keeps. With fewer other servers, it goes to as many as there are, and a single server keeps none elsewhere: the panel says that the only copy then is off-site, or none.
- **Off-site, to one destination,** an existing `backup.destination` the owner picks in Settings → Backups. The control plane uploads it itself, as it holds the destination's credentials, to its own prefix in the bucket.
- **The off-site copy is the one to trust.** An agent can't open what it keeps, but one that's taken over can delete it, keep an older one, or not send one back. So the overview and Settings → Backups show how old the newest off-site bundle is, first and largest, with the agents' copies under it, and an install with no destination is a warning there until it has one. A restore shows the time of the bundle it's given beside the newest off-site one it can see, and says when it's older.
- **Retention:** each place keeps the newest 24, and one a day for the 7 days before, and drops older ones after a newer one is whole there. A run of failures drops nothing.

### How often

- **After the store changes, and at least hourly.** A write to the store starts a wait: a bundle is taken a minute after the last write, or 10 minutes after the first one if writes go on, and never more than once every 10 minutes. With no writes, one is taken each hour, so a place that missed one catches up.
- **Every bundle goes to every place it should.** Each place says, on Settings → Backups and the overview, when it last got one, and one that's more than two hours behind is a warning, with why.
- **The cost is small.** A bundle is the store's size, a few MB to tens of MB, sealed in a stream and written to a temporary file, never held in memory whole. An agent keeps at most 31.

### Restoring

- **`dockwell control restore` runs on the server that becomes the control server,** any server that joined or a new one. On one that runs the agent build, the install script installs the full build with `--restore`, and runs it. It takes a bundle from that agent's own copies, a file, or a destination (asking for its credentials), and the recovery passphrase. It refuses a bundle of a newer schema than its build, and a target that already has a store.
- **It writes the store and the key,** into SQLite, or Postgres given `--database` and an empty database, then mints the store a new generation (below), records that it was restored from the bundle taken at its time, and starts the control plane. The server's own agent keeps its identity, as its server is in the store.
- **Agents find it.** A control plane reached by a domain needs its DNS pointed at the new server, and nothing else. One reached by an address can't be found on its own: the restore prints, for each server in the store, `sudo dockwell agent repoint <url>`, which an agent takes only if what answers there is signed by the CA it already trusts. So the install script and the restore both recommend a domain for the control plane's address, and say why: with one, a restore is a DNS change; without, it's a command on every server.
- **The old control server stays down.** If it comes back, it runs the store as it was and agents still pointed at it obey it. So the store's generation counts: every restore makes it one higher, an agent remembers the highest it has seen, and an agent refuses a control plane whose generation is lower, and says so. One not yet repointed can't know, so the procedure says to stop the old control server first, and `dockwell control demote` stops one that comes back.

### Reconciling with what agents hold

- **The store has a generation:** a number and an ID, made at the install and moved on by every restore and rebuild. The welcome says it, and an agent's hello says the generation its records were kept under.
- **An agent from an older generation is reconciled, resource by resource,** as it first connects:
  - **The store has the same thing live there:** the record is kept again under the new generation, at the store's revision, so its revision floor means something again.
  - **The agent holds what the store doesn't know,** or at a later revision: it ran after the backup, and what runs wins, within limits. The control plane recalls the record from the agent, with its values sealed to the stream, and brings it back as a change that adopts it, with no step, as nothing on the server changes: "Brought back from hz-1 what it ran after the backup". A variable or secret the record pins at a revision the store lacks gets a revision of its own, holding the value the agent had, and the record is pinned to it. The change is on Activity and can be undone, which is then a change with steps.
  - **It's adopted on its own only when nothing else can be reached through it:** the record gains no host access over what the store has of the resource, the resource is placed on that server alone, or, unknown to the store, no other agent holds it, and every variable and secret it brings back is used by nothing placed elsewhere. Anything else waits, listed on the overview and Activity as "brought back, needs a look", until a person applies it with a passkey just now, or leaves it.
  - **The store has something live there that the agent doesn't hold:** it's filled, as ADR 0054 has it.
  - **The agent holds something the store's latest revision placed elsewhere or deleted:** the restored store is older than that move or deletion, so the agent's record wins as above.
- **Once its records are all reconciled,** the agent's generation is the store's, and fills and forgets go on as ADR 0054 has them.
- **An older agent reads nothing after a restore.** A restored control plane can't know what it gave a server after the backup, so of an agent that doesn't write a container's references in (`Hello.writes_references`), it reads, plans and applies nothing until that agent is updated, whatever the store says it names (#120).

### Rebuilding with no backup

- **What agents hold is enough to run what they run.** A new install, with a new CA, can take a server's records: `dockwell join --bring-back` joins a server and offers its records, opened under its old server's binding and sealed again under its new one. The control plane makes, in one change on Activity, the projects, environments and resources the records name, by the IDs and names they hold, with their values as variables and secrets, and places each on that server.
- **What's lost** is all the store held beyond what runs: people, passkeys, tokens, history, Activity, backups' records, schedules, what ran nowhere. The rebuild says so. It's for when the passphrase or every bundle is gone.

### What a server that's taken over can do

- **Its own bundles:** delete them, keep older ones, or not send them back. The off-site copy is the one to trust, and the panel says so.
- **After a restore, say it runs anything.** What one agent says it ran can't be checked against anything, as the store doesn't know it. So it's adopted on its own only when it reaches nothing that server didn't already reach, and stays on that server: a record that gains host access, such as a mount of the server's root or a privileged container, or that would reach other servers through a shared service, variable or secret, waits for a person. Otherwise a server taken over before a restore could put a privileged spec into a service that also runs elsewhere, and the next deploy would carry it to the fleet.
- **Not its records' values.** A record's values are what that server already ran with, so bringing them back gives the control plane nothing that server didn't have; a secret used elsewhere still waits for a person, as its new value would reach the other servers on their next deploy.

### Who may do what

- Setting the destination, showing the recovery passphrase, and setting one, take the owner and a passkey just now. Seeing where bundles are and when they were taken is an admin's. Taking one now is an admin's, and is on Activity. Applying what was brought back and needs a look takes an admin and a passkey just now.
- A restore and a rebuild take root on the server they run on, and the passphrase, or the agents' records. The control plane can't be restored over the API.

### Numbers

The agent protocol's fields for this are in 190 to 199: the welcome's generation, the hello's generation of its records, a call to keep a bundle and its result, a call to recall a record and its result. Event fields too are in 190 to 199, for a bundle taken, sent, or failing, and a restore. The public API's methods for backing up the control plane are a file of their own.

## Consequences

- Losing the control server loses at most the minutes since the last bundle, on two servers and off-site, and what agents ran since comes back from them.
- The recovery passphrase is one more thing the owner has to keep, as a destination's is, and losing it with every bundle leaves only a rebuild.
- A restored control plane is the same CA to every agent, so nothing has to join again; only an install reached by an address needs its agents repointed.
- Two control planes can't both lead the same agents: the generation fences an old one from every agent that has seen the new one.
- After a restore, what a server alone ran comes back on its own, and anything that reaches further waits for a person, so a restore can leave a list to look at.
- It comes in three slices: bundles, taken and kept on agents and off-site, with Settings → Backups; restoring, with the generation, repointing, and reconciling with agents, proved in the Docker job; and rebuilding from agents' records.
