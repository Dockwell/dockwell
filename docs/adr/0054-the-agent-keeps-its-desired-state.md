# 0054. The agent keeps its desired state, and puts back what goes missing

- Status: Accepted
- Date: 2026-11-07

## Context

PRD principle 5 says the control plane is optional at runtime: if it goes down, servers keep serving and reconcile to their last desired state. The PRD's non-functional table makes that a launch gate, and [ADR 0007](0007-one-binary-two-roles.md) promised it: agents cache their desired state and reconcile locally. Issue #32 asks for it, done when restarting an agent with the control plane down restores every workload, crashed containers are restarted locally, and the cache is integrity-checked and encrypted where it holds secrets. `dockwell local` (#33) and the control plane's backup (#34) build on it.

What holds today, and what doesn't:

- **Docker already keeps most of it running.** A container's restart policy is `unless-stopped` unless its spec says otherwise, so Docker restarts one that crashes, and starts it again after a reboot. The ingress keeps its last config and starts with it, and puts back a copy a power cut damaged.
- **Nothing puts back what Docker can't.** A container that's gone, removed by hand, by `docker system prune`, or with Docker's data, comes back only when the control plane applies it again. Neither does one Docker tried to start and couldn't, as after a reboot where a disk mounted late, or a network that's gone. With the control plane away, nothing on the server knows what should be there.
- **The agent hears every step, and keeps none.** `agent.Desired` is told the spec of each step the agent applies and verifies, and nothing sets it yet.
- **A step's values reach the agent for that step alone.** The values of the variables and secrets a spec uses travel sealed to a key the agent keeps for one stream, and are cleared when the step ends ([ADR 0022](0022-secrets-encrypted-at-rest.md)). A container made again needs them. Docker keeps them, in the clear, in the container's own configuration on the server's disk, which the [threat model](../security/secrets.md) counts as root's to read; once the container is gone, so is that copy.
- **The firewall leans on this.** ADR 0052, in #288, has the agent record a spec as what it should hold only once it's confirmed, so its cache of desired state never re-applies rules that were rolled back, and puts the last confirmed table back as it reconciles.

## Decision

### What the agent keeps, and where

- **One record for each resource it runs:** the spec the last step applied and verified on this server, the revision and change that step ran toward, the values of the variables and secrets the spec uses, at the revisions it pins, the name of its scope, which its containers are named after ([ADR 0035](0035-containers-named-after-their-environment.md)), and for a service or a container the image it runs, by its ID, and by its digest where it came from a registry.
- **In the agent's data folder,** `<data>/agent/desired/`, one file for each resource, named by a hash of its kind, scope and name. The folder is `0700` and each file `0600`, owned by the agent's user. A file is written as the ingress writes its config: to a temporary file, synced, renamed over the old one, and the folder synced, so a power cut leaves the old record or the new one.
- **Each record is sealed whole,** with AES-256-GCM under the cache's key, `<data>/agent/desired.key`: 32 random bytes made on first use, `0600`, never sent anywhere and never in a backup Dockwell takes. The additional data is the format's version, the server's ID and the resource's kind, scope and name. So a record reads only with the key, only for that resource on that server, and one that was edited, torn or copied from elsewhere doesn't open.
- **That is the integrity check.** The agent opens every record as it starts. One that doesn't open, or is of a format this agent doesn't know, as after going back to an older agent, is moved to `desired/damaged/`, logged, reported when the control plane returns, and never acted on. A lost key leaves every record unreadable, which reads as an empty cache: the agent makes a new key, and the control plane fills it again (below).
- **Why the values are kept.** Without them nothing that uses a secret, which is most of what has a database, can be made again while the control plane is away. Each record holds only the values of what runs on its server now, which [ADR 0022](0022-secrets-encrypted-at-rest.md) already trusts that server's agent with, and Docker keeps the same values in the clear beside it. So this changes ADR 0022's "the agent writes none" to: the agent writes them sealed, in this cache, and nowhere else. A value leaves the cache when its record changes or goes, as a rotation or a deletion has it.
- **What the sealing does, and doesn't.** The key is on the same disk, so sealing keeps the values out of a copy of the folder that hasn't the key, and makes damage show; it doesn't hide them from root, who reads them through Docker anyway. Holding the key in a TPM or the kernel's keyring can come later without changing the records.

### When a record is written

- **Only for what passed.** A record is written once a step has applied on this server and passed: verified, and for the firewall, confirmed (ADR 0052). That is where `agent.Desired` hears of it. A plan writes none, nor a step that failed, nor one still running. A step that deletes a resource removes its record once it's done.
- **Versioned by revision.** A resource's revisions only grow, as an undo and a rollback write new ones that hold older specs ([ADR 0009](0009-resources-revisions-and-plans.md)). The agent keeps a record for a revision at least the one it holds, and refuses an older one, so a late or repeated message can't put the past back. A restart, a restore or a backup acts on a resource as it is, at the revision it has, and writes the same record again.
- **The control plane fills what's missing.** An agent from before this, one reinstalled, or one whose cache was lost or damaged holds less than it runs. So its hello lists what it holds, each resource with its revision, and how many records didn't open, which can't say what they were. For each resource whose latest revision is placed on this server and was applied there, and which the agent doesn't hold at that revision, the control plane sends it (`RememberCall`), its values sealed to the stream as a step's are, through the same check the keeper makes for a step: only the latest revision placed on that server, and only what it pins. That changes nothing on the server, so it is no change and no revision. Where the agent holds a later revision than the store has recorded as applied there, as when a step ended after its stream did, the control plane sends nothing: resuming that change decides, as it does now.

### On its own, while the control plane is away

- **The control plane leads while a stream is up.** Once the agent has had no stream for 60 s, from its start or from the last one ending, it reconciles from its records: then every 60 s, and as Docker reports one of its containers dying or going, until a stream opens. What it is doing then finishes, and nothing new starts.
- **It asks each resource's driver for a plan, and takes only what destroys nothing.** It goes in the order a plan does: networks, volumes, then services and containers, then routes. For each record it plans as a step would, without asking anything to run:
  - **Made again:** a network, or a service's or a container's container, that's gone, from its record, with its values, and from its image by ID. If that image is gone too, it's pulled by its digest. It's never pulled by a tag, which may have moved since.
  - **Started:** a container Docker tried to start and couldn't, which Docker leaves with an error in its state, when its policy says it should run (`unless-stopped`, `always`, or `on-failure` that hasn't run out). It tries again after 10 s, then waits twice as long each time, up to 5 min. Docker restarts what crashes, and the agent doesn't race it.
  - **Put back:** config that holds no data. The ingress already starts with its last config. The firewall puts back its last confirmed table, as ADR 0052 has it.
- **What it never does alone:**
  - Remove, replace or change in place anything. A container that differs from its record stays as it is: that is drift, which the control plane shows when it returns, and #33 adds adopting or reverting it.
  - Make a volume again. An empty one would hide that the data is gone, and Docker would make one for a container that mounts it, so a missing volume, and every container that mounts it, waits, and says why.
  - Start a container someone stopped, which Docker leaves with no error, or one whose policy says it stays stopped: `no`, or `on-failure` that ran out.
  - Finish, undo or roll back a change. Each resource stays at the last revision a step applied and verified, even when that leaves a change half done, and the control plane resumes it when it returns, as it does now.
  - Touch a resource with a step still running, or one whose record didn't open.
  - Anything while a stream is up.
- **It writes down what it did,** in `<data>/agent/desired/journal`: each entry the resource, what it did or why it couldn't, and when, never a value or a spec. It keeps at most 500 entries, the oldest going first.

### Handing back

- When a stream opens, the hello lists what the agent holds and how many records didn't open, and the agent sends its journal. The control plane puts each entry on Activity and the server's timeline, as something the server did while the control plane was away, and says it has them, and the agent drops those entries.
- Then the control plane fills the cache, as above. Its drift check shows what the agent left as it was.

### What it costs

- **Memory:** an index of the records, each its resource and revision, about 160 bytes: a test opens 1,000 records with 8 KiB of spec and values each, and the index holds 158 KiB. A record's spec and values are read only as reconciling needs that record, and dropped, the values cleared as a step's are. While connected, nothing runs beyond writing a record after a step. While away, one goroutine with a timer, and the Docker event stream the agent already reads. `make budgets` goes on measuring the idle agent against the 40 MB budget ([ADR 0018](0018-keep-the-budgets-in-rss.md)).
- **Disk:** a record is its spec and values, a few KiB, and a value is at most 64 KiB.
- **Numbers:** the agent protocol's fields for this are in 160 to 169, and the timeline's in event fields 140 to 149.

## Consequences

- Workloads come back with no control plane, from what their server held, those that use secrets included, and a server says what it did once the control plane is back.
- The values of what runs on a server are on its disk twice: Docker's copy in the clear, and the cache's, sealed. Moving secrets into files on tmpfs, as the threat model plans, has to take the cache's copy with it, and wants the key off the disk.
- A server can stay halfway through a change for as long as the control plane is away. That is what was verified there, and the control plane takes it from there.
- A volume that's gone stops what mounts it until someone acts, where making it again would have run on empty data.
- `dockwell local` (#33) reads the cache for status, and keeps the revisions before for its rollback, under the same rules. A lost control plane's state can be rebuilt from what agents hold (#34).
- It comes in three slices: the cache, written after each step and filled by the control plane; reconciling while away, and handing back; and the proof in the Docker job, with the control server stopped, a container removed and one that can't start, the agent restarted, everything back, and Activity saying so when the control server returns.
