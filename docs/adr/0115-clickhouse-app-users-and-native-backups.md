# 0115. ClickHouse app users and native backups

- Status: Accepted
- Date: 2026-11-27
- Amends [ADR 0033](0033-backups-of-volumes-and-databases.md), [ADR 0038](0038-databases.md) and [ADR 0106](0106-restoring-a-backup-to-a-new-database.md).

## App users

A ClickHouse link can create a database and user in the same plan. The app gets database-scoped access without backup privileges, grant option or a server-wide role. Its query thread and memory limits are fixed. Each linked app uses its own account. The agent requires a password-protected administrator before making the account. Dockwell saves the generated password in a sealed secret and sends ClickHouse its SHA-256 hash. The app reads its credentials by reference, so rotation updates the account and dependent apps in one plan. Removing the user leaves its data in place. A new account can use that database by the same ClickHouse UUID. Disconnect the app before removing its user.

The connect sheet selects an app user by default when it can verify the administrator's password is required. It offers to set a password when the app user option is unavailable. A person can choose the administrator and see that it can read and change every database. Existing administrator connections keep working. Only one link per ClickHouse service can use the administrator. The public API keeps that default for existing callers and takes `dedicated_user` for an app account.

## Backup and restore

Running ClickHouse makes a single native `BACKUP DATABASE … TO File(…)` archive while writes continue. It includes the databases outside the system and information schemas and the SQL access entities that hold users, password hashes and grants. A stopped ClickHouse gets a cold copy of the volume, including when a backup runs on a schedule. The native ZIP is encrypted with a random password for that run and staged outside the SQL user-files directory. ClickHouse's backup `File` engine can name it, but a restore needs the password. Dockwell sends the password over stdin and truncates ClickHouse's recorded query text before the password. The agent re-packs the encrypted ZIP as its usual verified gzip tar and removes the staged file. A later attempt removes a stage left by an interruption. Remote storage uses the same sealed format as other database backups. ClickHouse must require explicit grants to read system tables.

A one-GB data volume should have about 1.2 GB free for its encrypted stage and another 2.4 GB where the agent reads that ZIP and writes its archive, plus the usual disk reserve. When the data and backups are on the same disk, that disk needs about 3.6 GB free plus the reserve. Archive entries stream through fixed-size buffers. The agent keeps the ZIP directory in memory. The database still needs memory for its normal workload and native backup workers, which run within its container limit.

Restore re-encrypts the verified archive with a fresh password and loads it using ClickHouse's `RESTORE`. An in-place restore verifies the administrator can create users before replacing any data. Restore to a new database loads it into a fresh volume using a loader with no network, then starts the catalog install on that volume. The backup includes SQL-managed access entries. The new project gets its own copies of the original catalog secrets via ADR 0106. The original service and data are available during the restore. An open MergeTree table's parts are backed up at a consistent point during concurrent inserts. Independent writes to several tables do not become a multi-table transaction through a backup.

## Upgrade

Dockwell offers the pinned ClickHouse 26.3 LTS image through the catalog upgrade flow. It allows forward moves of less than a year in place. A move to a different major version requires a native backup in the plan, even when asked to skip backups. The next step requires the agent to report the completed ClickHouse dump. A mount at the standard ClickHouse data path identifies a mirror at plan time. Before a major replacement the agent verifies that the old and new containers use the same managed data volume. The image check compares Docker image IDs so a moved tag also needs verification. The agent checks the actual releases and verifies that Docker created the replacement from the checked image before starting it. It refuses a planned downgrade or a release it can't identify. If the new release fails its health check, the rollout starts the old release again on the same data.
