# 0033. Backups of volumes and databases

- Status: Accepted
- Date: 2026-11-06

## Context

Issue #88 asks Dockwell to back up an app's volumes and databases, because moving an app off another panel loses that panel's backups. A backup that can't be restored is worse than none, so this record starts from the restore.

What was settled already:
- [ADR 0009](0009-resources-revisions-and-plans.md): every change is a change with a plan, and a plan that destroys data says so. What destructive actions remove stays for 7 days.
- [ADR 0016](0016-scoped-api-tokens.md): no API token may apply a destructive plan.
- [ADR 0020](0020-the-event-timeline.md): what happened outside a change comes to the timeline from the audit log.
- [ADR 0021](0021-the-docker-driver.md): a volume is a Docker volume, in an environment, under Compose's labels.
- [ADR 0022](0022-secrets-encrypted-at-rest.md): revealing a secret takes a person, a session and a passkey.
- PRD principle 6: no lock-in, so a backup has to be usable without Dockwell.

## Decision

### Where a backup lives, and what it is called

- **On the server that has the volume,** at `<data>/agent/backups/<environment ID>/<volume>/<backup ID>.<format>`, with `<backup ID>.json` beside it saying what it is. Directories are 0700 and files 0600.
- **A backup's ID is `bkp_…`,** like every other ID. Its environment and volume are in its path, and its time in its record and its `.json`.
- **Plain formats.** A copy of a volume is `.tar.gz`, with numeric owners, modes, times, hard links, device nodes and extended attributes, so POSIX ACLs and capabilities too. A dump is `.sql.gz`. Each opens by hand with `tar --numeric-owner -xzpf` or `gunzip | psql`, with nothing of Dockwell's installed.
- **Not encrypted.** A backup on the server's disk is as readable as the volume it came from, to whoever is root there. The directory modes keep other users out, and nothing more. The panel says so. Only a copy sent to S3-compatible storage is sealed.
- **The control plane keeps a record, and the server keeps the bytes.** The `backups` table says what each backup is, for lists that work with the server offline. The file and its `.json` are the truth about what is on the disk.
- **A backup outlives its volume.** Deleting a volume leaves its backups. Each stays until someone deletes it.

### How a volume's bytes are read

**In a throwaway container, with nothing else in it.** A volume holds whatever its app wrote. An agent running as root that walks that tree on the host can be led by a symlink or a swapped directory into the host's own files. Inside a container with only the volume mounted, there is nothing else to reach.

- **The helper is the agent's own binary.** The agent imports it into Docker as a one-file image, `dockwell-volume-tool:<run>`, runs it with `__volume-tool`, and removes both. So a backup pulls nothing, works offline, and needs no registry or third-party image. The binary is static, so it runs alone.
- **It runs as root, with no network and a read-only root filesystem.** Docker mounts the volume, whatever its driver, so NFS and plugin volumes work as local ones do. It's mounted read-only for a backup.
- **Bytes travel over Docker's attach stream,** with the container's logging off, as otherwise Docker would keep a second copy in the container's log. The agent writes them to `<backup ID>.partial`, and renames that only once it has read the archive back to the end.
- **A file that can't be read fails the backup.** Docker's own archive endpoint logs such a file and carries on, which is why this doesn't use it, beside having no way to empty a volume.

### What a backup takes

| Volume | With the app running | With it stopped |
| --- | --- | --- |
| The data of a known database that's running | The database's own dump. Nothing stops | An exact copy of its files |
| Anything else | A person chooses: stop the app for an exact copy, or copy it running | An exact copy of its files |

- **A known database** is the official `postgres` or `mysql` image with the volume holding its data directory: the volume is the deepest mount at that directory or above it. The directory is `PGDATA` when the container has it, as postgres:18's image does, and the image's default otherwise. Any other volume of that container, such as an archive beside it, is "anything else": a dump holds none of its files, and restoring one would load over a database in another volume. PostgreSQL uses `pg_dumpall --clean --if-exists` and MySQL `mysqldump --all-databases --single-transaction`. Other images, MariaDB and Redis among them, are "anything else" until they have a dump and a test that restores it for real.
- **A copy of a running app's files is worth what the app makes of it.** Files change while they are read, so the copy can hold half a write, and what opens fine in a test can fail on the day. The panel says so, and defaults to stopping the app. The record says which way it was taken, and how many files changed along the way.
- **Stopping is a word first, then a stop.** The panel names what stops. `CreateBackup` stops only with `stop` set. What was stopped starts again whatever came of the backup. The agent notes it on disk first, so it starts again if the agent dies partway.
- **A dump is checked at its end.** The process has to exit 0 and the stream has to end with the words the tool ends a complete dump with.
- **A dump is consistent for transactional tables.** `--single-transaction` holds a snapshot of InnoDB's. A MyISAM table written to during the dump may not come out consistent, and the sheet says so.
- **A backup needs room before it stops anything.** It takes the last backup's size, or before there's been one what's in the volume, plus a fifth, and leaves 5% of the disk alone, between 256 MB and 2 GB. Without that room it's refused, with the numbers, and writing watches the disk too. How many backups pile up is for retention, with schedules. A backup taken by hand stays until someone deletes it.

### What it is to the revision store

- **A backup is an event, not a change.** Taking one changes no desired state, so it makes no revision. It's a row of `backups` and the audit events `backup.taken`, `backup.failed` and `backup.deleted`, which go on the timeline as `EVENT_KIND_BACKUP`. So a failure shows on Activity.
- **A restore is a change with a plan, and no revision.** It changes no spec, as a restart doesn't, so it runs through the same path: `Edit.Restore` plans a step on the backup's server, and a token in the change means it can't happen twice. The step destroys data, so a person has to apply it.
- **The plan says what it replaces, what stops and for how long,** in `PlanStep.restore`: the backup, when it was taken, how it was taken, and each service that stops. The fingerprint covers it, so a service that starts using the volume after the plan was reviewed means reviewing it again.

### What a restore does

1. It checks the backup, and that there's room, before anything stops: for step 3's copy where backups go, and for what comes back in the volume. Where both are on one disk, for both. Without it the restore is refused, with the numbers, and nothing was touched.
2. It stops what mounts the volume, and for a dump, everything else of the app on the server, as a client mid-write would break the load.
3. It copies the volume as it stands, with everything stopped, to `before-<change ID>.tar.gz`. The server keeps that for 7 days from the end of the restore.
4. **A copy** unpacks into a directory inside the volume, and only once all of it is there, moves the old files out and the new ones in, and removes the old ones. A restore that fails before then leaves the volume as it was.
5. **A dump** empties the volume, starts the database so its image makes a new one from the environment, and loads the dump. So what was made since the backup goes too, and a broken data directory is no obstacle.
6. It starts what it stopped, the database first.

**A failed restore undoes itself, as a failed deploy rolls back.** Once the volume isn't what it was, any failure puts step 3's copy back and starts what stopped on that, and the change fails saying so. That covers a load that's out of time or cut short, on a context of its own with its own deadline, as an app must never start on half a load. It covers what won't start after the data was replaced too. If the copy can't go back, what stopped stays stopped, and the copy stays on the disk: the error gives its path and `dockwell changes undo <change>`, which is why a failed restore, alone among failed changes, can be undone. A change that resumes after a restore was cut short starts over from the copy.

- **Undo puts back step 3's copy,** as a restore of its own, for as long as the change's undo window lasts. Whichever kind of backup was restored, step 3's copy is files, so undo needs the same version of the database as the one that wrote them.
- **If the agent dies partway,** the change resumes as a restart does, and the `.json` of step 3's copy says how far it got. It never copies over a finished copy. A resume with a copy isn't checked for room, as a restore that's part done has used that room, and refusing it would only strand the app. Whatever else stops a resume goes the way of a failed restore, so what stopped starts again.
- **A dump restores across versions, and a copy of a database's files doesn't.** The plan says which it has, and when a copy goes under another release than the image that wrote it, an amber warning says the database may not start, and what to do. Passwords in a dump come back with it, so one changed since the backup goes back to the old one.
- **What a database says of a failed load stays out of the change.** It can quote the rows it choked on, and a read-only token can read a change. The error keeps its code and first line, without what it quotes.

### Who may do what

| Action | Read-only token | Read-write token | Signed-in person |
| --- | --- | --- | --- |
| List backups | Yes | Yes | Yes |
| Back up now | No | Yes, and it can stop what `stop` names, as a deploy can | Yes |
| Plan a restore | No | Yes | Yes |
| Apply a restore | No | No | Yes |
| Delete a backup | No | No | Yes |
| Download a backup | No | No | With a passkey, once downloads exist |

- **A restore needs a person** because it replaces data, as ADR 0016 has it for every plan that destroys data. Where the volume reaches what's as good as root, such as a bind of `/etc`, it takes a passkey too ([ADR 0032](0032-files-on-a-server.md)). An ordinary volume doesn't.
- **Deleting a backup needs a person** too, as nothing brings one back, and it is what a person relies on after something goes wrong.
- **A download will need what revealing a secret needs.** A backup holds an app's secrets in its data, with a database's password hashes and whatever the app stored. So a download will need a session and a passkey step-up for that backup, as `RevealSecret` does, no token will be able to make one, and each will be in the audit log. This slice has no download at all. You can fetch a file from the server as root.

### Schedules and retention

- **A schedule is a resource, so setting one is a plan.** Kind `backup.policy`, in the volume's environment: a volume, how often, when, how many to keep, and whether to stop the app for each. Undo and history come with it. It names its volume, so the volume can't go until the policy does.
- **The control plane keeps the schedule,** as it keeps the record of backups, so the kind runs on no server (`resource.Unplaced`). A run due while it's down happens once, when it's back: servers get 2 minutes after a start to connect, and only one still away past that fails its run. Runs due together on a server go one at a time, so each looks for room in what the last left. That's less than PRD principle 5 asks of what serves traffic, and a backup doesn't serve traffic. An agent keeping its own schedule can come later, with no change to what a policy says.
- **A run that can't happen says so.** A server that's away, or a disk with no room, fails that run's backup, with the reason, on Activity, under Dockwell's name. The policy goes on from there.
- **Retention is per policy and per server.** It runs before each run and after one that read back whole, and never goes below what the policy keeps, so a run of failures takes nothing away, and a schedule cut back comes down before a run is refused for room. A backup a planned or running restore names stays until that's over. A backup someone took by hand is theirs, and no policy deletes it. What goes is on the timeline.

### Room on the disk

- **A backup says what it will cost.** `PreviewBackup` gives the last one's size, or what's in the volume before there's been one, and what's free.
- **A backup never takes the last of the disk.** It leaves 5% alone, between 256 MB and 2 GB. It's refused before anything stops if the last one, and a fifth more, wouldn't fit, and it stops partway if it's about to go below that. Either way nothing is kept of it, and the failure says what to do. A restore's copy of what's there goes through the same check, so a full disk stops a restore before it touches anything.

### S3-compatible storage

- **A destination is a resource,** kind `backup.destination`, in the organization, so any environment's backups can use one. A policy or a backup names it. Its access key, secret and passphrase are `(sensitive)`, so its specs are sealed (ADR 0022) and a plan says only that they changed.
- **Setting one always plans as a change,** as a secret's value does, so a plan can't confirm a guess at it. For the same reason an update keeps the passphrase whatever the edit gave, and says so in the plan (`kept_passphrase`): every copy there was sealed with it, and refusing a change would say whether a guess was right. To use another, add a destination under a new name. One that's removed and added again under its name has the passphrase it's given, so the panel can take one you already have. Each backup records a keyed hash of what sealed it, which goes on no API, so a fetch that fails can say a copy needs an earlier passphrase.
- **What signs in goes to a server for one call.** The control plane opens the spec, and seals it to the agent's stream with the same HPKE as a step's values, bound to that server and volume. The agent opens it in memory, clears it when the call ends, and writes it nowhere. No request carries it: each is signed with Signature Version 4.
- **A backup is done when it's whole on its server.** Sending it is a step of its own, after that's on record and the volume's lock is gone, so an upload holds up no restore however long it takes, storage that's down costs a copy and not a backup, and a control plane that stops mid-send leaves a whole backup with a copy that didn't arrive. A copy that fails says so on the backup, on its schedule and on the timeline as `backup.copy_failed`, and the next backup goes on. A schedule whose destination was removed goes on too, with each backup kept on its server and saying so. A backup can't be deleted while its copy is on its way, as what lands would have no record. A request that goes 2 minutes without a byte moving, in either direction, is given up on.
- **It's sealed before it leaves,** so a provider, or anyone with the bucket, reads nothing but sizes, times and object names, which hold the environment's ID, the volume's name and the backup's ID. What's kept beside each backup is sealed too. A sealed file is a header, then chunks of 1 MiB under AES-256-GCM. The header says the format, a salt, the iterations and what the file is bound to, which is its environment, volume and backup. The key comes from the passphrase by PBKDF2-SHA-256 at 600,000 iterations, the standard library's, as `golang.org/x/crypto` isn't in the agent build and argon2id wouldn't earn it a place (ADR 0023). A random passphrase from the panel takes the same slow step: a header can't say a phrase needs none. Every chunk's associated data is the whole header, and its nonce is its number and whether it's the last, so nothing can be moved, dropped or cut short, a header can't be changed to ask for less work, and an older object under a newer name won't open. A part of an upload is a whole number of chunks, so a part that fails is made again from its place in the file, with no second copy on the disk.
- **Nothing but the passphrase opens one,** and it's on no server at rest. So the panel says to keep a copy somewhere that isn't this install. `dockwell open-backup` opens a file with that alone, in either build, with no control plane.
- **The agent has a small S3 client of its own,** in `internal/backup/s3`, with no SDK and no `encoding/xml`, as what it links costs every server (ADR 0023). It's checked against AWS's own worked signatures, and on a real S3 server, versitygw, in `scripts/docker-e2e.sh`.
- **`FetchBackup` brings one back** onto a server that has the volume, the one it came from or another once that's gone. The control plane sends the SHA-256 it has on record, and that's what it has to read back to: nothing from the bucket decides it. It's written under a name of its own, with the volume's lock held, and takes its place only once it matches, and a server that still has the backup refuses. A fetch that fails says so as `backup.fetch_failed`, on the backup and on the timeline. Then it restores as any backup does, so there's one path through a restore.
- **Deleting a backup takes it from both, or from its server alone.** The panel says which, and removing it from the server alone keeps it on its destination, to fetch from. A bucket that refuses doesn't keep the server's copy: that goes, the timeline says what the bucket said as `backup.copy_kept`, and it's tried again by a person, or by the next run. A destination that was deleted is the bucket's to look after: Dockwell has nothing to sign in with.
- **Retention looks at each place on its own.** A server keeps the newest it should, and so does a destination, which can be older ones: a copy goes only once that many newer ones are stored there. So with an expired key, a server moves on, the bucket keeps what it last had, and the backups that left the server stay on record, to fetch.

### Left for later

These come with the next slices of #88 or their own issues, and each is built on the above:
- a backup before a catalog upgrade;
- bind mounts, which aren't volumes, and websites;
- restoring to another server or a new volume, and restore drills.

## Consequences

- A backup depends only on Docker's API, so it works for any volume driver, and for an agent that doesn't run as root.
- Each run imports the agent's binary as an image for a moment, so it needs that much room in Docker's storage. A crash can leave one behind, which the agent removes when it next starts.
- A restore needs room for the copy of what's there as well as the backup, and for a copy, for the old and new files side by side in the volume. Without it, the restore is refused before anything is touched. A volume too large to measure in 10 seconds goes ahead on what the disk says as it's written.
- Backups on the server's own disk go with the server. That protects against a bad deploy, not a lost server, without a destination. The panel says so.
- Whoever loses a destination's passphrase loses what's in that bucket. That is what keeps it from the provider too, and Dockwell can't get around it.
- A database on an image Dockwell doesn't know is backed up as files, with the app stopped. That is exact, but it only restores on the same version.
