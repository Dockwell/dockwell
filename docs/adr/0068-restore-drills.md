# 0068. Restore drills

- Status: Proposed
- Date: 2026-11-10

## Context

A backup is only as good as a restore. The PRD lists restore drills, "scheduled test restores that prove backups work" (Data, P1), and #266 marks them missing (#478). [ADR 0033](0033-backups-of-volumes-and-databases.md) checks a backup as it's taken: a dump has to end with the words its tool ends a complete dump with, and a copy has to read back to its end. Nothing shows that it restores: that a database starts on it, answers, and holds what it held. Finding out on the day it's needed is the failure this prevents.

A drill runs beside the live app, on its disk, its memory and its CPU, so it must never be what takes the live app down, and a drill that couldn't run must never read as a backup that doesn't restore.

## Decision

### A drill restores a backup into a scratch container, checks it, and throws it away

- **On the server that has the backup,** into a container of its own and a scratch volume of its own. Both carry a label that marks them as a drill's, with the drill's ID. It never touches the live volume, the app or the app's networks. A backup that's only in its destination is fetched first, into a drills directory under the backups root, through the same SHA-256 check as `FetchBackup`, and goes with the drill. Each server a policy runs on drills its own newest backup.
- **It's new code, not ADR 0033's restore.** A restore empties the live volume and starts the live container again, with that container's own command, variables, networks and mounts (`internal/backup/restore.go`). A drill makes a container of its own. What carries over is waiting for the database to be ready (`awaitReady`) and loading a dump (`loadDump`).

### What the drill container is given: the image, not the service

- **The image** the database's container runs now, by its image ID, so a backup that won't restore under today's build fails its drill, which is worth knowing before the day. When the service is gone, the image the backup's manifest names, if the server still has it; otherwise the drill is not checked: "the image this backup was taken with is gone".
- **The image's own command,** with the drill's flags after it (below). Never the service's command line: one such as `postgres -c config_file=/etc/postgresql/postgresql.conf` names a file the drill doesn't mount, and would fail a drill of a backup that's fine.
- **Only the image's setup variables,** each password replaced by one made for the drill and kept nowhere: `POSTGRES_USER`, `POSTGRES_DB`, `PGDATA` and `POSTGRES_INITDB_ARGS` with its `--auth`, `--auth-local` and `--auth-host` flags taken out; `MYSQL_ROOT_PASSWORD` and `MYSQL_DATABASE`. None of the service's other variables, which can hold what its app signs in with, and which a restore doesn't need.
- **One mount, its scratch volume,** where the live container mounts its data (`dataMount`). No config file or `conf.d` mount, so a live `shared_buffers` or `innodb_buffer_pool_size` sized for the server doesn't get the drill killed under its memory limit.
- **Sealed off:** network `none`, nothing published.
- **Durability off,** as nothing it writes is kept: PostgreSQL with `-c fsync=off -c synchronous_commit=off -c full_page_writes=off -c wal_level=minimal -c max_wal_senders=0 -c max_wal_size=256MB`, MySQL with `--skip-log-bin --innodb-flush-log-at-trx-commit=0 --innodb-doublewrite=0`. That roughly halves the load's writes to the app's disk, keeps WAL short and the binary log out, and changes nothing about what loads.
- A guard test, as `internal/backup/safety_test.go` is for backups, checks every drill's create request: network `none` and no port bindings, no mount but its scratch volume at the data directory, only the variables above, and a helper whose unpack writes only to the scratch volume.

### How it signs in after the load

The live passwords never reach the drill, as ADR 0038 keeps them from the agent, so the checks can't rely on a password once the backup's own users are in.

- **PostgreSQL** loads and checks over the container's local socket, which the image trusts, since the drill's `initdb` gets no auth flags. The dump's `ALTER ROLE ... PASSWORD` then changes nothing for it. A copy of a data directory brings its own `pg_hba.conf`, so the drill writes one that trusts the local socket into the scratch volume and starts with `-c hba_file=` naming it.
- **MySQL** loads a dump signed in as root with the drill's password. The dump ends with `FLUSH PRIVILEGES` over the backup's own `mysql.user`, which takes that password away, so the drill restarts the server with `--skip-grant-tables --skip-networking` and checks over its socket. A copy of a data directory starts that way from the first.

### What it checks

- **A dump** passes when the database starts, loads it within its time, answers `SELECT 1` and lists its databases, and holds what the dump holds. The agent reads that from the dump itself as it feeds it to the load: `\connect` and `USE` switch databases, `CREATE DATABASE` and `CREATE TABLE` name them, and a `COPY ... FROM stdin;` block with a row before `\.`, or an `INSERT INTO`, fills a table. The drill passes when every database and table the dump makes is there, and every table the dump fills has a row, which `SELECT 1 FROM <table> LIMIT 1` reads with a page each. What today's image brings of its own, such as MySQL's system schemas or PostGIS's `spatial_ref_sys`, is more than the dump made and doesn't count against it. That needs no record at backup time, works on every dump taken already, costs the live database nothing, and catches a table whose rows were lost. The agent keeps at most 100,000 tables of a dump in mind, and past that checks those and says so.
- **Sizes are information,** never a pass or a fail: "4.1 GB restored, 9.8 GB live". A reload drops bloat, so a database that's fine can come back at half its size, and a small one that lost every row stays within any bound. This is decided for review, and the owner can overrule it.
- **A copy of a PostgreSQL or MySQL data directory** passes when the database starts on it and answers `SELECT 1` and its list of databases. A copy under another release says so, and how to fix it: "a copy of PostgreSQL 16's files doesn't open under 17. Restore it under 16, or back it up as a dump".
- **A copy of anything else** passes when it unpacks with no error, with the bytes the manifest recorded (`Manifest.Unpacked`) and the number of files the read-back counted, which the backup keeps from now on rather than throw away. It shows as "Unpacked whole", not "Restored and checked": it proves the archive restores whole, which the read-back mostly showed already, not that the app makes sense of it. MariaDB, Redis and Valkey are copies under ADR 0033 until they have dumps, so their drills are copies' drills until a later slice starts the engines `internal/database` already asks (`engine.go`) on the copy. Redis refuses a damaged RDB or AOF as it starts, which is a real check.
- **What the database printed** before it stopped, its last lines through `scrubbed`, goes with a failed or unchecked drill's result, as the container that printed them is thrown away.

### Three results

- **Passed.**
- **Failed:** the backup didn't restore, or failed a check. "The dump didn't load: ...", "orders.items has no rows; the dump fills it". Only this fires the critical rule.
- **Not checked, with why:** cut short, refused or stopped for room or memory, killed for its memory, past its time, the stream to the agent ended, or the image is gone. It says nothing about the backup. It shows on the backup, and a scheduled drill tries again at the policy's next run. The control plane's `Recover` marks drills that were running when it started as not checked, as it does backups.

### It stays out of the live server's way

**Disk,** where the drill writes:

- Before it starts, it needs room for what comes back on the disk the scratch volume lives on, which is Docker's data root and can be another disk than the backups root. The agent measures that through the helper, as `roomToRestore` does, and the backups disk for a fetched file, counting both once when they're one disk. It needs the backup's unpacked size, a fifth more for the temporary files of index builds, and the most its log holds, 256 MB of PostgreSQL's WAL or MySQL's redo log with its binary log off, above ADR 0033's floor of 5% of the disk, between 256 MB and 2 GB, and below the `disk-full` rule's threshold, 90% by default, so a weekly drill never sets that warning off. Without the room, it's not checked, with the numbers.
- While it runs, the agent watches that disk every few seconds, as a backup's writer does through `keepRoom`, and stops the drill at the floor, as the database writes its bytes and the agent doesn't.
- **Backups win.** What the drill may still write is its reservation, and every backup's and restore's room check counts it as taken. A backup or restore that hasn't room beside it stops the drill and goes on, and near the floor the drill's watch stops it before a backup's `keepRoom` would stop the backup.

**Memory:** the drill's limit is the service's, or 1 GiB where it has none. Before it starts, `MemAvailable` has to cover that and 256 MB more, or it's not checked. It runs with `OomScoreAdj` 1000, so when the server runs short the kernel kills the drill, not the live database, and an OOM kill (`docker.WentPastMemory`) is not checked: out of memory.

**CPU:** `CpuShares` 256 against Docker's 1024, so the live app wins when they compete, and at most one CPU, and at most half the server's CPUs.

**Time:** the restore's own estimate from the backup's size (`estimate`, a dump at 4 MB/s), four times over, as a load rebuilds indexes and checks constraints, at least 10 minutes and at most 6 hours. Past that it's not checked: "took longer than 2 h". That also tells someone how long a real restore takes.

**No live hold:** a drill reads only the backup's file, so it takes neither the agent's hold on the live volume, which refuses a backup or restore rather than wait, nor the control plane's per-server turn for scheduled runs. It has a slot of its own: one drill at a time on a server.

**Leftovers:** the agent sweeps drills by their label, never by a name, before each drill and every hour with `keepPruned`, removing each with `RemoveOneOff`, so the anonymous volume its image declares goes too, and clears the drills directory. The idle check `Recover` makes counts a running drill, so a sweep as Docker comes back doesn't remove one that started first. The Containers call skips the drill's label, as it does the volume tool's.

### When

- **On demand,** "Check it restores" on a backup, `BackupService.DrillBackup`.
- **On a schedule,** as part of the backup policy: `BackupPolicy.drill`, `DRILL_NEVER`, `DRILL_WEEKLY` or `DRILL_MONTHLY`, where unset (`DRILL_UNSPECIFIED`) reads as never. A weekly drill checks the newest backup that read back whole, an hour after the policy's own time on its weekday, Sunday for a daily or hourly policy; a monthly one an hour after the month's first run. The policy sheet says when a drill ends at the longest.
- **New policies drill weekly,** through `resource.NewDefaulter`, which runs only for a resource that's new and not for an undo, so editing a policy never turns drills on. Catalog installs and the 1Panel importer make new policies, so theirs drill weekly too. The plan and the policy sheet say what a drill costs before anyone applies it: "about 12 GB and 40 minutes each week".
- **Existing policies stay as they are.** Backups shows a callout, "3 schedules never check a restore · Check weekly", which plans that edit for all of them, for a person to review and apply, with history and undo. Neither the upgrade nor anything else makes it silently: a drill costs disk and I/O that the owner of an existing policy didn't plan for. This is decided for review, and the owner can overrule it.
- Retention keeps a backup while it's being drilled, as it does one a restore names, and deleting one is refused until its drill ends.

### What it is to the revision store, and to Activity

- **The schedule is a field of `backup.policy`,** so setting it, changing it or turning it off is a plan with a revision, history and undo, as any policy change is. Nothing new calls `Revise`.
- **A drill is an event, not a change,** as a backup is: it changes no desired state, so it makes no revision. It's a row of `backup_drills` (the backup, who or which policy started it, when, how long, the result, why, and what failed in one line, without what a database quotes of its data, as ADR 0033 keeps a failed load). Rows go with their backup.
- **The audit event `backup.drill`,** with its result, which stays on Activity once the backup is gone. It reaches Activity under `EVENT_KIND_BACKUP` once it's one of `backups.Actions`, and `EventBackup` gains the drill's result.
- **Where it shows:** on the backup's row and the database's Backups tab, "Restored and checked 2 days ago", "Restore check failed 2 days ago: the dump didn't load · Check again", or "Not checked: the disk had no room · Check again", and on the volume's newest backup in the Backups list.

### How drills alert

- **`restore-drill-failed`,** critical, never repeated, `CONDITION_RESTORE_DRILL_FAILED`: the newest backup of a volume, on a server, that has been drilled failed its drill. It resolves when a backup at least as new passes; deleting the failed backup doesn't resolve it alone. A drill of an older backup shows on that backup's row only, so last month's backup passing doesn't hide this week's failing, and an old copy from before an upgrade doesn't page anyone.
- **`restore-drill-late`,** warning, `CONDITION_RESTORE_DRILL_LATE`: a policy that drills has gone two of its periods without a pass, whether its drills were not checked or never ran. `backup-late` reads backups' runs only, so it doesn't cover that.
- Both are made for every install, as ADR 0044's defaults are, with a row in its table. `MakeDefaults` stops today on an install whose `alerts.defaults` is set, which is every install that has alerts, so it drops that stop and relies on `EverManaged` alone: a new default reaches an install that upgrades, and a rule someone deleted stays deleted.

### Who may

- **Running one** is as "Back up now" (`CreateBackup`) is, exactly: a developer of the backup's project, an admin in a protected environment, and a read-write token. It reads a backup into a container nothing reaches and replaces nothing, so it needs no passkey. It's on the audit log with who started it, and a token starts at most one drill of a volume an hour.
- **Seeing the results** is whoever can list the backups.
- **Changing the schedule** is changing the policy, a plan, with the roles a policy change has.

## Consequences

- The agent gains a call, `DrillBackup`, agent protocol 220 in `Call` and `CallResult` and `Hello.drills` 221, and the agent build gains nothing to link: it runs the database's own image and its own volume tool. Its memory holds a dump's table names, at most 100,000.
- Backups keep the number of files their read-back counts, and the store gains `backup_drills`.
- The policy sheet gets one line, "Check a restore: weekly · Change", Backups the callout for policies that never check, and each backup its state and "Check it restores".
- It lands in two slices. First, drills on demand, with their record, their three results, their checks, everything above that keeps the live server safe and the leftovers sweep, and `restore-drill-failed`, since on-demand drills use the same disk and memory as scheduled ones. Then the policy's schedule, the weekly default for new policies, the callout, and `restore-drill-late`.

### Not in these slices

- **A drill of the stored copy.** A lost server leaves only the copy at the destination, and only a drill of it shows that its passphrase and seal still open it. A monthly drill could fetch and open that copy, or stream it through opening and SHA-256 with nothing written. S3 charges for the egress, R2 doesn't. It's a record of its own once slice 2 lands.
- **What a drill doesn't prove:** that the backup restores on another server or under another release; that an app makes sense of its files; or the restore path's own stop, swap and undo, which their tests cover. A backup that passed can still hold the wrong data, such as one taken after the data was lost.
