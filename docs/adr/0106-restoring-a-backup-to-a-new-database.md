# 0106. Restoring a backup to a new database

- Status: Proposed (2026-11-18), after an independent review (five mediums, six lows) and a re-check (one high, two mediums), fixed on the branch.
- Amends:
  - [ADR 0033](0033-backups-of-volumes-and-databases.md), whose restore only replaced what a volume holds;
  - [ADR 0080](0080-updating-an-app-safely-and-automatically.md), whose database move this restore borrows its loader from.
- Depends on:
  - [ADR 0084](0084-what-an-app-needs-met-by-a-new-service-one-you-run-or-a-connection.md), whose link backups keep restoring in place (slice 2 below);
  - [ADR 0068](0068-restore-drills.md), whose drills check a backup restores without keeping what they restore.
- Date: 2026-11-18
- Issue: #830

## Context

A restore replaces what's in a volume. For a database that means everything that uses it stops while the backup loads, and what was written since is gone. The usual question behind a restore is "what did this table hold on Monday", which that answer is too heavy for: a person wants to look at the old data beside the new, copy back what they need, and only then decide.

The Backup-Restore mock offers two choices, with the first recommended: **To a new database**, and **Replace**.

## Decision

### One change makes the new database and fills it

`RestoreBackup` with `into` (a project name) plans **one change** that installs the original's catalog entry as the project `into`, on the server that holds the backup, and restores the backup into the new database's own volume. The database it was taken of appears nowhere in that change.

- **What the new database runs** is the original's catalog pin as it runs: its Compose file (images by digest), values, release and companions, through the same `PlanInstall` a catalog install uses. It isn't the catalog's latest, so a copy of files opens under the series that wrote it.
- **Where:** the backup's server alone, with the daily backup schedule a catalog install gets (#793). Its display name is the original's plus " (restored)".
- **Its ports:** the original runs on the same server and holds each port it publishes, such as Adminer's. Each port the file publishes from a variable, in any form (`${NAME}`, `$NAME`, `${NAME:-8081}`, or a long mapping's `published`), moves to the next one no service or container Dockwell runs has taken, passing over the ranges the servers say they lend to outgoing connections, as the catalog reads them for an install (#688). A variable with no value takes the file's default. A `restore_port` note in the plan says which moved: "ADMIN_PORT is 8082, as the database it was taken of publishes on 8081".
- **Which backups:** a whole volume's backup of a database entry's database, installed from the catalog, done and on its server, whose install runs with nothing other apps give it (no pin needs).
  - **Only a database entry.** Its services are databases Dockwell knows, as its database page does (PostgreSQL, MySQL, MariaDB, MongoDB, Redis and Valkey), and the entry's own companions, such as Adminer. A second copy of an app would act on whatever its secrets reach, a bot token or a mailbox, beside the first. So these are refused as `not_a_database`, with "Replace its data instead":
    - a backup of an app's own volume, on the control plane, in the engine and in the agent;
    - a backup of the database an app ships beside itself, on the control plane. The panel offers the new database only for an entry the catalog files under databases.
  - **The database it was taken of** is the service that keeps its data in the backup's volume. After a move to a new release (ADR 0080), that's the volume of the new name, while the catalog's file names the first. A backup from before the move, of a volume nothing runs on now, is of the environment's one database.
  - A volume with no pin is refused as `no_pin`. A backup of one app's database (a link's) keeps `restore_from_app`.

### The restore rides on the new service's create step

A dump loads into a running engine, and the new volume has none until its service starts. So the restore isn't a step of the volume, as an in-place restore is: it's attached to the **create step of the service** that mounts the volume, as ADR 0080's database move is.

- **The step:** the service's step carries `Restore` with `into_new`. Its agent, before it makes the container, loads the backup into the new service's **own data volume**: the one it mounts at its engine's data path, as the database page reads it (`database.FromSpec`), whatever the backup's volume is called. After a move to a new release the original's volume is `pgdata-17` while the file names `pgdata`, which is what the new database fills.
  - a **dump** in a loader of the new database's own release, with its environment, command and user, on no network (`helperDatabase`, as the move's);
  - a **copy of files** unpacked into the volume (`unpack`).
- Then the service's container is made and starts on what the backup holds. Nothing else of the step changes.
- **One step per ref** keeps the engine's ordering, rollback and progress as they are.

### Where the backup is read from

`RestoreSource.from` (agent field 4) names the volume the backup was taken of. The step's own plan doesn't carry it, so the control plane passes it at apply in the `ApplyCall`'s `desired.restore`, as it does at plan.

The agent reads a backup only from **that volume's own backups**:
- `from` must be a volume, and its environment and name must each be one folder name: not empty, `.`, `..`, nor holding `/`, `\` or NUL.
- The backup's ID must parse as a backup's (`bkp_`).
- Every folder between the backups root and the file must be a folder, not a link, and inside the root (`Lstat` and `filepath.Rel`).
- The manifest and the file must be regular files, not links; the manifest's ID must be the one asked for, its format known, and the file's size the manifest's.

The new service's environment, its data volume and the change's ID are checked as folder names too, before any path is made of them, and the new environment must not be the one the backup was taken in.

### Guards: the original is never written to

- **The engine** (`intoNew`, at propose) refuses an into-new restore unless its item is a database the change makes, in an environment other than the backup's, and its own data volume is new in the same change. A service or a volume that's here already, the original's environment, and an app that isn't a database are refused.
- **The agent** refuses a new service that isn't a database it knows, one in the original's environment, and (`seedIsNew`) a volume that anything mounts or that holds data the restore didn't put there. It refuses a step that isn't a create, unless this change's own restore is what made the database (below).
- **The original's backup is only read.** Its volume, container and revision aren't in the change.

### Secrets: copies of the original's, on the server

The dumps carry the original's roles and their password hashes (`pg_dumpall`, `mariadb-dump --all-databases` with `mysql.user`), and a copy of files carries everything. So the new database's own secret inputs take **the original's values**: without them its Connect password wouldn't sign in to the data it restored.
- **They're copied on the control plane,** from the original's secrets into the new project's own secrets, as the install's `Secrets`. No value is in a response, and the plan names them only: "takes copies of its POSTGRES_PASSWORD".
- **Its own copy.** The new database's Connect details read its own secret, never a shared one, so rotating one later never changes the other.
- **On the activity log.** Each copy is a `secret.copy` event, with the original's secret as its target and "copied POSTGRES_PASSWORD from warehouse into chg_…, to restore it as warehouse-restored". It's written in the transaction that puts the planned change on record (`Proposal.Records`), so there's never a plan holding copies without its record, and a request refused before planning copied nothing.

### Failing safely

- **A backup that's damaged** is refused as it's planned, before anything is placed, within the plan's call: the read takes the call's context and stops when the call ends.
  - Up to 2 GiB, `JudgeSeed` reads the whole file and checks its hash and its end.
  - A larger backup has its gzip header and first 64 KiB read as it's planned, as a minute's plan can't read it all on a small server. The whole of it is checked as it loads, before anything is written, where a damaged one fails and the change rolls back.
  - The load always checks the whole file before it writes.
- **A load that fails** empties the new volume (`emptySeed`), so the rollback doesn't keep a half-restored copy for 7 days. The change fails, and its rollback removes the new project as it removes what any failed change made.
- **A later step that fails** after a good load, such as the new database's health check or Adminer's port, rolls the change back too. The new volume is kept for 7 days, as any deleted volume is. Nothing empties a volume on a rollback: a rule keyed on a restore having once filled it would empty a restored database that later changes brought back, with weeks of use in it.
- **The restore's note** (`restore-into-<change>.json`, in the new volume's backups folder) says how far the load got, `filling` or `done`, while the step runs. It goes once the step that made the database is done, so nothing reads it after.
- **A run cut short** keeps going:
  - before the container is made, the step is a create and loads again, emptying first;
  - once it's made, as a restart of the control plane or a dropped agent during a later step leaves it, the resumed plan has the step go on where the restore finished, loading nothing again: the note says `done`, or, once it's gone, a container of the new service carries this change's ID. Where the note says `filling`, the step fails, so the change rolls back rather than stay applying. A finished step with nothing left to do passes the engine's restore check on a resume.
- **Undo** of an into-new change is an ordinary undo: it deletes what the change made. `restoring()` skips into-new restores, so undo never plans a restore. Deleting the new volume is destructive, so the undo's plan asks for its fingerprint.
- **The item has a revision.** `noRevision()` treats an in-place restore as acting on what's there, but not an into-new one, which makes its service.

### The plan and the panel

- **The plan isn't destructive:** no stops, no data destroyed, no typed fingerprint. The step says "Loads the dump from 6 Nov, 03:00 into db on hz-1", and "The database it was taken of keeps running as it is". The data row reads "from the backup", not "replaced".
- **The Restore sheet:** Restore on a backup's row, its page, and "Restore the newest" on a database's Overview open it, for a backup the server would accept, and for someone who may make a project (`may(installCatalogEntry)`, an admin). It shows the restore point, then **Restore to**: To a new database (chosen, Recommended, with `<name>-restored` filled in, or the next free name), and Replace `<name>`, which goes on as today, with the shared server's typed name. The name is checked as it's typed: against the projects there are at once, then by the control plane (`GetCatalogEntry`'s free name), which knows the names used before. Review plan opens the plan in the change sheet.
- **Everyone else, and other backups,** keep today's direct path to Replace: a developer can't make a project, and no new database can be made of other backups.

### Roles

- **RestoreBackup with `into`** needs both:
  - a developer on the backup's project, as its `REACH_PROJECT` rule binds the backup's ID;
  - and the install's admin, as making a project takes, which the engine checks before anything is recorded. A developer is refused as `needs_role`, with no `secret.copy` event.
- **So the copy reaches no one new.** Only an admin can trigger it, and an admin can already reveal the original's secrets (`RevealSecret`).

### Numbers

| Field | Number |
|---|---|
| `dockwell.v1.RestoreBackupRequest.into` (string) | 3 |
| `dockwell.v1.RestorePlan.into_new` (bool) | 11 |
| `dockwell.agent.v1.RestoreSource.from` (`ResourceRef`) | 4 |

No migration.

## What we turned down

- **A restore step on the new volume, after the service starts,** with today's `replaceWithDump`. It would start the new database empty, stop it, and load, and its "before" copy would cost a second copy of nothing. Loading before the first start is one step, and what the move already does.
- **A new random password re-set after the load.** It means re-setting each engine's root password, engine by engine, and can't cover a copy of files, which carries the password files themselves.
- **Restoring onto the catalog's latest release.** A copy of files only opens under the release that wrote it.

## Consequences

- A person can look at Monday's data beside today's, with nothing that runs touched.
- The new database costs its disk and memory until someone removes it.
- Its users come over as the backup has them. The Users tab reads them as users Dockwell didn't make.
- The real-Docker tests (`TestARestoreIntoANewPostgresLeavesTheOriginalOnRealDocker`, `TestARestoreIntoANewMariaDBLeavesTheOriginalOnRealDocker`) check the original's data, revision label, container and start time are unchanged after a restore and after one that fails partway, on CI's Docker job.

## Follow-ups

- **Slice 2: one app's database in a shared server** (a link, ADR 0084), restored as a new database of its own name in that same server, through `RestoreCatalogNeed`, as its own item. The agent loads a one-database dump under a new name: `pg_dump -d X` without `-C`, and `mysqldump "$1"` without `--databases`, load into any name. It also makes that database and a user for it, which is a call in ADR 0084's lane, with numbers asked for then. Until then, a link backup keeps restoring in place from its app's page.
- After a restore, an "Open warehouse-restored" link where the change applies.
- **Kept volumes listed and removable.** A deleted volume, including a rolled-back restore's, stays on its server for 7 days, and nothing lists it or removes it sooner (ADR 0036). That takes an API of its own, with its own numbers.
