# 0080. Updating an app: one plan, safe by default, and automatically for those who opt in

- Status: Proposed
- Date: 2026-11-12

## Context

On 2026-11-12 the owner asked what upgrading an app is like, and asked for it to be smooth and robust. [ADR 0079](0079-knowing-a-newer-version-of-an-app-is-out.md) (#545) decides how Dockwell learns that a newer version is out and where it says so. This record decides what happens once a person, or Dockwell on their behalf, takes it (#546).

What there is to build on:

- **A catalog install upgrades already.** `UpgradeCatalogInstall` plans a three-way merge of the old entry's file, the new one and what runs ([ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md)), backs up the volumes the release keeps first, and its undo puts the old version back, not its data. The panel offers it only on the catalog entry's page.
- **An image app deploys another image.** `DeployService` plans running another image with the rest as it is. A deploy of the tag it runs pulls again, and replaces only if that's newer ([ADR 0021](0021-the-docker-driver.md)).
- **The pull comes first.** `container.Docker.run` pulls before it rolls, so what runs keeps running until the new image is on the server, and the downtime is the restart alone.
- **A rollout is health-gated and rolls back on its own** ([ADR 0027](0027-health-checks-rollouts-and-rollback.md)). A rollback pins the digest the old revision ran, and says why, in the deploy's own words.
- **A backup can come first.** `BackupFirst` steps run before anything else in a change and stop it if they fail ([ADR 0033](0033-backups-of-volumes-and-databases.md)). A database is dumped while it runs; anything else is copied with what mounts it stopped.
- **Step progress is words.** A step says "Pulling nginx:1.27: 12 of 48 MB", which the panel shows as it is.
- **Database majors have rules only for MongoDB** ([ADR 0038](0038-databases.md)). PostgreSQL 16 can't read the data 17 needs and the other way round, so changing `postgres:16` to `postgres:17` today makes a container that won't start, and the rollout puts 16 back. MySQL and MariaDB upgrade their data in place on start, one release series at a time, and never back.
- **One change applies to a resource at a time** (`revision.ErrBusy`), and an applying change resumes after the control plane or the agent restarts.

What's missing: one flow for every kind of app, a plan that says what an update is (from what, to what, what's backed up, what stops), progress a person can follow, a disk check, a refusal of database moves that can't work, updating several apps, and doing it unattended.

## Decision

### One flow: `UpgradeService`

`POST /v1/services/{id}:upgrade` (`dockwell services upgrade`) plans updating one service, as any plan is planned: a change, ready to apply or discard. It takes the version ADR 0079 found when it's given no image, or the image it's given. What it plans depends on where the service came from:

| The service | Its update |
| --- | --- |
| Runs an image, alone or from a Compose file | A deploy of the newer image, with the rest as it is |
| Is part of a catalog install | The install's upgrade to its newer release, as `UpgradeCatalogInstall` plans it, for the whole install |
| Builds from Git | Refused: a deploy builds its branch's newest commit ([ADR 0046](0046-builds-from-git.md)) |
| Runs from a folder | Refused: `ChangeServiceRuntime` moves it to another version ([ADR 0058](0058-runtimes-an-app-from-a-folder.md)) |

A tag is deployed as a tag, not pinned to a digest: a version tag names one release, and a moving tag that moved again before Apply moved to something newer, which is what an update is for. What each server pulled goes on record as the step's outcome, as for any deploy.

**An update says what it is.** Its plan carries an intent of its own kind, `INTENT_KIND_UPGRADE`, with the image and digest it leaves and the image it goes to, and for a catalog install the release it leaves and the one it goes to. The panel and the CLI say "Update web from 16.3 to 16.4" from that, while it's reviewed, while it runs and on Activity.

### Safe by default

**The pull before anything stops.** As built. A pull that fails leaves what runs running.

**Room for the image before the pull.** Docker fails a pull that fills its disk partway, after minutes, and may leave the disk too full for anything else. So before it pulls an image the server hasn't got, to replace a container that's there, the agent compares the space free where Docker keeps images with what the new image will take, which it reads as the size of the image it replaces plus a quarter, as a new release is about as large as the last, plus 5% of the disk kept free, between 256 MB and 2 GB, as a backup keeps it. Short of that it refuses, with the numbers, before anything is pulled or stopped. The step's reason is `disk`. A first deploy, with nothing to compare with, is left to Docker, whose error says the same.

**A backup first, by default.** An update backs up each volume Dockwell made that the service mounts, before anything else, unless it's asked not to (`skip_backup`). A database's volume is dumped while it runs, and anything else is copied with what mounts it stopped, which the plan names with its downtime. A server path can't be backed up, and the plan says so by name. Leaving the backup out is a switch in the panel, and the plan says so in one line.

**A health-gated rollout that puts itself back.** ADR 0027, as built: a check that fails rolls back to the last revision that deployed, pinned to the digest it ran, and says why, with the logs one click away.

**A database's major release is never a plain swap.** For PostgreSQL, MySQL and MariaDB, a change of the release series an image runs is refused:

- **When it's planned,** from the tags: PostgreSQL's major (16, 17), MySQL's and MariaDB's series (8.0, 8.4; 10.11, 11.4). A tag that names no release, such as `latest`, or names less than a series, such as `mysql:8`, can't be read there.
- **As it's applied,** from the images themselves, after the pull and before anything stops: the official images say their release in their environment (`PG_MAJOR`, `MYSQL_MAJOR`, `MARIADB_VERSION`), so a moving tag that crossed a major is refused there, with the old container running as it was.

Why MySQL and MariaDB too, though they upgrade in place: their data moves one way. Once MySQL 8.4 has started on 8.0's data, 8.0 can't read it, so undo and the automatic rollback, which put the old image back, would leave the database down; and neither skips a series. The safe way across is a dump and a restore into a new volume, with the old one kept as it was, so undo is exact. It's the next slice of this work, built on ADR 0033's dump and restore: until then the refusal says it isn't built yet. MongoDB keeps ADR 0038's rules.

**Undo** goes back to the revision before, which pins the digest that ran, as a rollback does. The plan says in one line that it doesn't take back what the new version did to its data, and names the backup that does.

### Progress a person can follow

A step says what it's doing as a phase as well as in words: `STEP_PHASE_BACKING_UP`, `STEP_PHASE_PULLING` with the bytes done and the bytes in all, `STEP_PHASE_STARTING` and `STEP_PHASE_CHECKING`, which the agent sends beside its message (`CallProgress` 260 to 262) and the API passes on (`StepProgress` 260 to 262). The panel draws an update as those phases in order, with the pull's bar, in the plan sheet and on the app's page, and ends "Updated to 16.4", with Undo; "Rolled back to 16.3", with why and the logs; or failed, with why. An older agent sends words alone, and the panel shows the words.

### Several at once (slice 2)

Selecting rows on the Apps list or the Overview plans one update for each app, each its own change, applied together. So each rolls back alone, and one app's failure never undoes another's update. The panel shows them as one action with a row each, as the Updates tab does for packages, and "Update all patch updates" selects the patch updates.

### Automatic updates, opt-in per app (slice 2)

- **The service's spec says it,** in `auto_update` (Service 170 to 179), so turning it on is a change like any other, on Activity and undoable: the level (off, the default; patch; minor and patch; any), the window (days and a time of day, and how long it lasts), and whether to back up first, roll back on failure (both on) and tell a channel ([ADR 0044](0044-alerts.md)).
- **Dockwell applies it as its own change.** Within the window, the control plane plans `UpgradeService` for each service whose update ADR 0079 found and whose level takes it, as Dockwell, and applies it. A plan that would need a person (two writers, what reaches the host, a destructive step) isn't applied, and the channel says so. A failure rolls back as any does, and alerts. A database's major release is never automatic, nor is a version a person skipped.
- **An admin sets the default for new apps** in Settings.

### Robust

- One update per app at a time: `ErrBusy`, as built.
- An update that's applying resumes after a restart of the control plane or the agent, as every change does.
- A server that's offline waits: an automatic update for it isn't planned until it's back, and a window that ends first waits for the next.
- Registries' limits are ADR 0079's to keep for checks; an update's pull is Docker's own.
- **The image an update leaves is kept for its rollback** (slice 2). Docker leaves a moved tag's old image with no tag, which `docker image prune` removes. So the agent tags what it replaces under a name of its own and removes those tags after 7 days, the undo window, with nothing else touched.

## Consequences

- An image app, a Compose service and a catalog install update through one button and one plan, and the CLI's `services upgrade` does the same.
- A database's major move that used to fail at start, and roll back, is refused before anything stops, and a moving tag that crosses a major no longer takes a database down.
- An update with volumes takes longer by its backup, and a volume that isn't a database's stops its app while it's copied, which the plan says. `skip_backup` leaves it out.
- `StepProgress` gains a phase and bytes, which every deploy fills, not only updates.
- ADR 0079's record of a found update is what an update with no image takes. Before it lands, `UpgradeService` needs an image.
- Still to come: the dump-and-restore move across a database's major release, updating several, automatic updates, and keeping the image an update leaves.
