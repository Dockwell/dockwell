# 0068. Restore drills

- Status: Proposed
- Date: 2026-11-10

## Context

A backup is only as good as a restore. The PRD lists restore drills, "scheduled test restores that prove backups work" (Data, P1), and #266 marks them missing (#478). [ADR 0033](0033-backups-of-volumes-and-databases.md) checks a backup as it's taken: a dump has to end with the words its tool ends a complete dump with, and a copy has to read back to its end. Nothing shows that it restores: that a database starts on it, answers, and holds what it held. Finding out on the day it's needed is the failure this prevents.

## Decision

### A drill restores a backup into a scratch container, checks it, and throws it away

- **On the server that has the backup,** into a container of its own and a volume of its own, `dockwell-drill-<drill ID>`. It never touches the volume, the app or the app's networks. A backup that's only in its destination is fetched into the drill's scratch space first, through the same SHA-256 check as `FetchBackup`, and goes with it.
- **It restores as a restore would today.** A database starts from the image its service runs now, by digest, as step 5 of ADR 0033's restore does, so a backup that won't restore under today's image fails its drill, which is worth knowing before the day. A dump is loaded as a restore loads one. A copy is unpacked as a restore unpacks one.
- **It's sealed off.** No network (`--network none`), nothing published, no mount but its scratch volume, at most one CPU, and the service's memory limit, or 1 GiB where it has none. The database gets a password made for the drill, which is kept nowhere. Its labels mark it as Dockwell's drill, so drift, the Containers list and the app's health never read it, and an agent that starts removes any drill it finds left.
- **It's always thrown away,** container and volume, whatever the result. What's kept is the record.

### What it checks

What a backup held is recorded as it's taken, from now on: for a database, each of its databases with its number of tables and its size; for a copy, its number of files and its bytes. A drill compares against that.

- **A database** passes when it starts, loads within its time, answers a query as its own client (`SELECT 1`, then the list of databases), has every database the backup held with the same number of tables, and a total size between half and twice what it measured at the backup (a load rebuilds indexes and drops bloat). Rows aren't counted: that would read every table.
- **A copy of anything else** passes when it unpacks with no error, and its files and bytes are exactly what the backup held. That proves the archive restores whole. It doesn't prove the app makes sense of it, which only the app can say, and the panel says that much.
- MariaDB and Redis are copies of files under ADR 0033 until they have dumps, so their drills are copies' drills, and a copy of a known database's data directory gets a database's checks.
- A backup taken before its contents were recorded gets the checks that need no record: it starts, loads and answers, or unpacks whole.

### When

- **On demand,** "Check it restores" on a backup, `BackupService.DrillBackup`.
- **On a schedule,** as part of the backup policy: `BackupPolicy.drill`, never, weekly or monthly. A weekly drill checks the newest backup that read back whole, an hour after the first run on or after the policy's weekday, Sunday for a daily or hourly policy; a monthly one after the first run of the month. A new policy drills weekly unless set otherwise. One that exists stays as it is until someone changes it, as a drill costs disk and CPU its owner didn't ask for; the panel says "Never checked · Check now" on its backups meanwhile.
- **One drill at a time on a server,** after any backup or restore of the same volume, and retention keeps a backup while it's being drilled, as it does one a restore names.

### What it costs, and its limits

- **Disk:** room for what comes back, the size the backup recorded plus a fifth, and ADR 0033's floor of 5% of the disk, between 256 MB and 2 GB. Without it, the drill doesn't run, and says so with the numbers.
- **CPU and time:** at most one CPU while it runs, and the I/O of a load on the disk the app uses, which is why a scheduled drill goes an hour after its backup, at the policy's quiet time. It gets four times as long as its backup took, at least 10 minutes and at most 6 hours, and past that fails as not finished in time.
- **What it doesn't prove:** that the backup restores on another server or under another release; that an app makes sense of its files; or the restore path's own stop, swap and undo, which their tests cover. A backup that passed can still be one that holds the wrong data, such as one taken after the data was lost.

### What it is to the revision store, and to Activity

- **The schedule is a field of `backup.policy`,** so setting it, changing it or turning it off is a plan with a revision, history and undo, as any policy change is. Nothing new calls `Revise`.
- **A drill is an event, not a change,** as a backup is: it changes no desired state, so it makes no revision. It's a row of `backup_drills` (the backup, who or which policy started it, when, how long, the result, and what failed in one line, without what a database quotes of its data, as ADR 0033 keeps a failed load), and the audit event `backup.drill`, which goes on Activity under `EVENT_KIND_BACKUP`.
- **Where it shows:** on the backup's row and the database's Backups tab, "Restored and checked 2 days ago", or "Restore check failed 2 days ago: the dump didn't load · Check again", and on the volume's newest backup in the Backups list.

### How a failed drill alerts

A rule made for every install, as ADR 0044's are, once for an install that upgrades: `restore-drill-failed`, critical, never repeated, when a volume's newest drill failed or couldn't run. It resolves when a later drill of that volume passes. A drill that's late isn't a rule of its own: `backup-late` already covers a schedule that stopped.

### Who may

- **Running one** is as "Back up now" is: a developer of the backup's project, and a read-write token. It reads a backup into a container nothing reaches and replaces nothing, so it needs no person and no passkey; it is on the audit log with who started it.
- **Seeing the results** is whoever can list the backups.
- **Changing the schedule** is changing the policy, a plan, with the roles a policy change has.

## Consequences

- The agent gains a call, `DrillBackup`, agent protocol 220 and 221 in `Call` and `CallResult`, and the agent build gains nothing to link: it runs the database's own image and its own volume tool.
- Backups record what they held as they're taken, and the store gains `backup_drills`.
- The policy sheet gets one line, "Check a restore: weekly · Change", and each backup the state and "Check it restores".
- It lands in two slices: drills on demand, with their record, their checks and the alert rule; then the policy's schedule.
