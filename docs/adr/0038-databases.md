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

### A database and its user

*Added with the second slice of #207.*

- **A database and the user who owns it are a resource,** of kind `database.user`, in the environment of the service that runs their database server. Its name is the user's, and its spec names the service, the database, which takes the user's name unless it says another, and the secret the password is in. So making one is a change with a plan, it has revisions, history and an undo, and the engine already refuses what would leave one naming a service that isn't there, or isn't on its servers. A step that runs once with no revision, as a restore does, would have none of that, and nothing would say a user was Dockwell's.
- **It's made in the database server's own container,** as a reading is, with the same tools signing in the same way, on each server the service runs on. PostgreSQL makes a role that signs in and owns a new database; MySQL and MariaDB make a user at `%` with every privilege on its database. Names are lowercase letters, digits and underscores, starting with a letter, at most 32, which every engine takes as they are, so nothing a person types is quoted into SQL.
- **The password is generated and kept as a secret,** by `DatabaseService.CreateDatabaseUser`: 32 characters from 62, which every engine, URL and shell takes as they are. One change makes the secret and the user, so nobody types the password or sees it unless they reveal the secret, which takes a passkey (ADR 0022). An app uses it by name, as any secret.
- **The user uses the secret as a service does** (`resource.Binder`), so the agent gets its value sealed to its stream, for the step alone, and only on a server the user is placed on (ADR 0022). Setting the secret anew is a plan that takes the user in with every service that uses it, and gives the user the new password, which the catalog's own passwords can't do (ADR 0029).
- **The password never goes in as it is where a server could keep it.** PostgreSQL gets a SCRAM-SHA-256 verifier, made by the agent as PostgreSQL's own clients make one, so its log, if it logs statements, holds no password. MariaDB gets its own hash of it. MySQL takes the password as it is, over the tool's stdin, and leaves it out of every log it keeps.
- **Nothing in a database says what Dockwell gave a user,** so the agent keeps that beside its other state, in `database-users.json`, as it keeps what it took on (ADR 0031): the service, the database, the secret and its revision. A user that's gone from its server reads as gone, which is drift. One that's there with no record, as when the agent's data was lost, is refused by name as one Dockwell didn't make.
- **A user that's there already is refused,** as is a database, by name, before anything is made: a plan never takes over what someone else made.
- **Deleting one drops the user and their database,** with everything in it. The plan says it destroys data, so only a person applies it, with the plan's fingerprint. It isn't kept for 7 days, as a volume is: back the database server up first, which its page offers.
- **Redis and Valkey have no users of their own** here, so the page offers none.
- **The agent build links nothing new for it:** `crypto/pbkdf2`, `crypto/hmac` and `crypto/sha1` are there already, for backups and TLS.

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
- Setting a user's secret anew restarts the apps that use it in the same change, and nothing orders the user's step before theirs, so an app can come up a moment before its user has the new password, and connect once it has.
- A database whose image keeps its password somewhere other than these variables, as a Redis with `requirepass` in a config file does, reads as refusing the sign-in, and says which variables Dockwell tried.
