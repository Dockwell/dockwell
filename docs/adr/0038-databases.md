# 0038. Databases

- Status: Accepted
- Date: 2026-11-07

## Context

Issue #207 asks for the Databases section: every PostgreSQL, MySQL, MariaDB and Redis that Dockwell runs or has taken on, what each holds, and a way to make a database and a user in one. Both servers looked at run PostgreSQL, one beside pgAdmin, and the catalog installs PostgreSQL and MySQL.

What was settled already:
- [ADR 0009](0009-resources-revisions-and-plans.md): every change to managed state is a change with a plan.
- [ADR 0022](0022-secrets-encrypted-at-rest.md): a secret's value goes only to the servers that run what uses it, sealed to one stream, and no response, plan, log or event carries it.
- [ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md): a catalog install is a project of services, with its password a generated secret.
- [ADR 0031](0031-containers-dockwell-didnt-make.md): a container Dockwell didn't make is taken on as a service.
- [ADR 0033](0033-backups-of-volumes-and-databases.md): a backup knows a database by its official image and the volume at its data directory, and dumps it with its own tools, in its own container, signing in with what that container's environment holds.

## Decision

### A database is a service

- **Not a kind of its own.** A database is a service whose image is the official one of PostgreSQL, MySQL, MariaDB, Redis or Valkey: `postgres`, `mysql`, `mariadb`, `redis` or `valkey/valkey` on Docker Hub, with any tag or digest, with or without `docker.io/library/`. So a database has what every service has: plans, revisions and undo, health, logs, taking on, and the catalog's upgrades, and one taken on from another panel is a database the moment it's a service. A kind of its own would be a second way to run what a service already runs.
- **Only the official images,** as in ADR 0033. Others keep their data elsewhere and sign in otherwise, and what Dockwell runs in a container has to be what that image has. Bitnami's PostgreSQL, pgvector and the like are services like any other, until a later record adds each with a test on its real image.
- **Its data is the volume that holds its data directory:** the deepest volume mounted at that directory or above it. The directory is `PGDATA` where the spec sets it, and otherwise where the image keeps it: `/var/lib/postgresql/data`, or `/var/lib/postgresql` as postgres:18 asks, `/var/lib/mysql`, or `/data`. A server path holds data too, but it isn't a volume, so it has no backups, and the page says the data goes with the container.
- **The control plane tells them apart by their specs,** so the list answers with every server away. `DatabaseService.ListDatabases` says which database each runs, its volume, the port in its command or the image's, the secret its first user's password is in where it uses one, the services beside it in its environment, and the newest backup of its volume.

### What a database says of itself

- **It's read in its own container, with its own tools,** as a backup dumps: `psql`, `mysql` or `mariadb`, and `redis-cli` or `valkey-cli`, run with `docker exec`. Each signs in as the image does, with `POSTGRES_PASSWORD`, `MARIADB_ROOT_PASSWORD`, `MYSQL_ROOT_PASSWORD`, `VALKEY_PASSWORD` or `REDIS_PASSWORD`, which the shell in the container reads from its own environment. So no password reaches the agent, the control plane, or the command line Docker keeps for an exec. A query goes on stdin.
- **It reads about the database and nothing in it:** its version, its size on disk, or for Redis and Valkey in memory, its connections and the most it takes, how long it's been up, how much of what was read came from memory, its databases with their owners and sizes, and its users with whether each can sign in and do anything. No table's rows, and no password or hash. PostgreSQL answers as one line of JSON, which holds any name as it is, and MySQL and MariaDB in batch mode, which escapes a tab or a newline in a name. A reading lists 200 databases and 200 users at most, and counts the rest.
- **It's a read.** It changes nothing and needs no grant, so a read-only token reads it, as `ListContainers` is read. The names of databases and users are what plans show anyway.
- **It's asked for as someone reads, and kept 10 seconds,** so a page that refreshes beside the CLI runs one `exec` per database in that time. One call asks a server about all of its databases, each with 8 seconds and the server with 10, so one that hangs costs its own row. A server that isn't connected says so, and isn't kept, as it may be back a moment later. There's no history, so the numbers have no trend yet.
- **What a database says when it fails keeps its first line, without what it quotes,** as in ADR 0033, as a reading is there for anyone who can read the database. A sign-in it refuses says which variables Dockwell signed in with, which is what to check.
- **The agent build links `internal/database`,** which holds the engines, the queries and what reads their answers, and nothing else new (ADR 0023).

### MongoDB and ClickHouse

*Proposed for the PRD's last two engines (#266's Databases row), to build on the first slice once it lands, and on the second for their users.*

Each is a database as the others are: a service on its engine's own image, read, given users, backed up and restored in its own container with its own tools, signing in with what its environment holds. What differs is below. Facts about the images are from their sources as their libraries build them on 2026-11-08: `docker-library/mongo` and `ClickHouse/docker-library`.

**Which images, and where they keep their data**

- **MongoDB is `mongo`** on Docker Hub, running `mongod`, from 6.0. Its images have `mongosh`, `mongodump` and `mongorestore` of the server's own release; before 6.0 they have the old `mongo` shell, so an older one is listed, reads as too old to read, and names the release to upgrade to. Its data is at `/data/db`, or where `--dbpath` in its command says. A path set only in a config file the service mounts isn't in its spec, so that database reads as having no volume, as a moved data directory does now. Its port is 27017, or `--port`.
- **ClickHouse is `clickhouse`** on Docker Hub, **and `clickhouse/clickhouse-server`,** ClickHouse's own repository, which most Compose files name. Both are built from ClickHouse's sources with the same entrypoint, and CI runs each. It runs `clickhouse-server`, or options that start with `--`, which its entrypoint passes to the server. Its data is at `/var/lib/clickhouse`. Moving that takes a config file, so it isn't seen, as above. Its client uses the native port, 9000, and the page shows the HTTP port, 8123, too, as most apps connect there.

**Signing in**

- **MongoDB signs in as the root user its image made** in `admin`, from `MONGO_INITDB_ROOT_USERNAME` and `MONGO_INITDB_ROOT_PASSWORD`, or their `_FILE` forms. Its entrypoint turns on `--auth` only when both are set. With neither, MongoDB checks no one, so the page says that anything that reaches its port signs in as anyone, and how to set them. `mongosh` reads them from `process.env` in a script it's given on stdin. `mongodump` and `mongorestore` get them in a config file (`--config`, in the tools since 100.3) read from a descriptor the shell opens. So the password is never an argument, which `ps` shows anyone on the server.
- **ClickHouse's client reads `CLICKHOUSE_USER` and `CLICKHOUSE_PASSWORD` itself,** the variables its image made its user from, so it runs with no shell and no password in its arguments. The image's distroless variant has no shell, and needs none. `CLICKHOUSE_PASSWORD_FILE` is read with `sh` where the image has one. With neither variable set, the image's `default` user has no password and signs in only from inside its container, as images have had it since early 2025, and the page says so. An older image lets it in from anywhere with no password, and the page says that too.

**What it says of itself**

- **MongoDB:** its version and its feature compatibility version (below), how long it's been up, its connections and the most it takes, its size on disk, how much of what was read came from WiredTiger's cache, whether it's a member of a replica set, its databases with their sizes, and its users with their database and roles. `usersInfo` across databases holds no credentials unless asked for them, and Dockwell never asks. It answers as one line of JSON.
- **ClickHouse:** its version, how long it's been up, its connections and the most it takes, its size on disk from its active parts, how often its mark cache answered, its databases with their engines and sizes, and its users with where each may sign in from. It answers in `JSONEachRow`. `system.users` holds no password or hash.
- The same limits hold: 200 databases and 200 users at most, asked for as someone reads, and kept 10 seconds.

**A database and its user**

- **MongoDB's user lives in its own database,** with `dbOwner` on it, so an app signs in with that database as its `authSource`. MongoDB makes a database when something is first written to it, so a new one is listed once its app writes. The password goes in over stdin. Dockwell asks for SCRAM-SHA-256 alone, so MongoDB keeps only that verifier, and it leaves the password out of its logs. Dockwell won't make a user where `--auth` is off, as the user would guard nothing, and says which two variables to set. Deleting one drops the user and the database.
- **ClickHouse gets a hash, never the password:** `CREATE USER ... IDENTIFIED WITH sha256_hash BY ... SALT ...`, which the agent makes from the secret, as it makes PostgreSQL's verifier. Then `CREATE DATABASE` and `GRANT ALL ON <database>.*`. Users made with SQL are kept in the data volume's `access/`, so they go with its backups. The image's user may make users only with `CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1`. Without it, Dockwell refuses, and says to set it, which is a plan that restarts the server. Deleting one drops the user and the database.
- **Everything else is as for the others:** a resource of kind `database.user`, a generated password kept as a secret, names Dockwell checks before anything reaches the database, a step that stops halfway finished by the next, nothing taken over that someone else made, and a delete only a person applies.

**Backups and restores**

- **MongoDB:** `mongodump --archive`, the whole server with its users and roles, gzipped by the agent as the others are. A member of a replica set is dumped with `--oplog`, so the dump is one moment, and restored with `--oplogReplay`. A standalone server keeps no oplog, so its dump isn't one moment if something writes while it runs, and the backup says so. A restore runs `mongorestore --archive --drop`, which replaces each collection the dump holds and leaves any other. It goes into the same release or a newer one.
- **ClickHouse:** no tool of its own dumps a whole server and reads it back, so Dockwell uses its `BACKUP`. It backs up every database but `system` and the information schemas, with users and roles, to one `.tar` under `/var/lib/clickhouse/backups`. That's the one place the image's default config lets `BACKUP` write. The agent copies the archive out with Docker's archive API, so nothing more has to be in the image, and a helper on the volume, as ADR 0033 runs one, removes it. So a ClickHouse backup needs room twice: in its own volume for the archive until it's copied out, and where backups go, which on one disk is both at once. What a backup will cost (`PreviewBackup`), and the backup step of an upgrade's plan, show that as a warning, with the volume's free space beside what the archive will take: the last backup's size, or before there's been one what's in the volume. A backup that can't fit is refused before it starts, with the numbers, as ADR 0033 refuses one with no room where backups go. Each table is one moment, and the server as a whole isn't. A restore puts the archive back there, drops each database it holds, and runs `RESTORE`, into the same release or a newer one.
- **ADR 0033's line holds:** a listed database is one Dockwell backs up with its own tools, and no engine ships until a test restores it on its real image.

**Upgrades**

- **MongoDB moves one major release at a time.** Each move starts from a feature compatibility version equal to the release it's leaving: 6.0 to 7.0, 7.0 to 8.0, and 8.0 or 8.3 to 9.0. So a change to its image that skips one is refused, and names the release to go through. An upgrade leaves the feature compatibility version where it was, so its undo is the image alone, which MongoDB supports. The next upgrade raises it first, with `confirm: true`, as the first step of its plan, which says the release before can't then run that data. The page shows it beside the version.
- **ClickHouse upgrades in place, to a release less than a year newer,** as ClickHouse asks. Its versions name their year and month, so Dockwell reads that from the tags. A longer move is refused, and names a release between. Its undo is the image too, which works while nothing new has been used, and the plan says so.
- **Both take a backup first,** as an upgrade does for any database (ADR 0033).

**Proven on the real images**

- CI's Docker job runs `mongo` at 6.0 and at its newest, and `clickhouse` at the oldest LTS its library builds and at its newest, plus `clickhouse/clickhouse-server`. On each, it lists and reads the database, makes a user that signs in over the network and not with a wrong password, backs up, restores into a new server, and moves one release up and back.
- The agent build links nothing new. SHA-256 for ClickHouse's hash is there already, and copying a file out of a container, or into one, is two more calls in `internal/docker`.

### The panel

The Databases section is built to `Databases-List` and `Database-Detail`, at `/databases` and `/databases/<service ID>`. A database shows its connection string with the password masked, and names the secret that holds it, where there is one. The page reads the database again every 15 seconds while it shows. Where it differs from the mocks:
- **No point-in-time recovery.** The design puts it in phase 4. The backups panel shows the newest backup of the database's volume, as Backups takes it.
- **The numbers have no sparkline,** as nothing keeps a database's history yet. The mock's fourth number, slow queries, needs an extension PostgreSQL doesn't have on by default, so the page shows how long the database has been up instead.
- **A server holds several databases,** so the page lists them all with their sizes and owners, where the mock shows the users of one.
- **No external access switch.** A database publishes no port (ADR 0029), and adding one is a port on the app's page.
- **Containers Dockwell didn't make that run a database** are named on the list, each a link to its page, where it's taken on. That's where the servers looked at start.

## Consequences

- A database taken on from another panel, or installed from the catalog, is listed with nothing more to do.
- A database on an image Dockwell doesn't know is a service, and isn't listed. That's the same line ADR 0033 draws for a dump, so a listed database is one Dockwell can back up with its own dump.
- Reading a database runs its client in its container, which costs a process for a moment on each read, at most once in 10 seconds for each.
- A database whose image keeps its password somewhere other than these variables, as a Redis with `requirepass` in a config file does, reads as refusing the sign-in, and says which variables Dockwell tried.
