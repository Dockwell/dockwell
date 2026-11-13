# 0084. What an app needs, met by a new service, one you run, or a connection

- Status: Proposed. Amends [ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md), where an app that needs a database brings its own and an entry can't use another app's, and [ADR 0022](0022-secrets-encrypted-at-rest.md), where a service uses only its own environment's values.
- Date: 2026-11-12

## Context

On 2026-11-12 the owner asked for apps that connect: WordPress with a new MySQL or MariaDB, or one they already run, or a managed database from a provider, with Variables and secrets as the bridge and the compatibility checked for them (#569). What's true today:

- **An entry brings its own database.** WordPress runs MariaDB beside it, Nextcloud PostgreSQL, each in the app's project, with a generated password the database and the app share by reference (ADR 0029). There's no other way to install them.
- **The design drew the choice and it was never built.** The `Catalog-Install` mock says "Ghost needs MySQL 8", offers Create, and Use an existing database, which it greys out with why: "`postgres` runs PostgreSQL, which Ghost does not support."
- **Projects can't reach each other.** Each project has its own networks, and a database publishes no port (ADR 0029), so an app in one project reaches neither a database's name nor its port in another.
- **A service uses its own environment's variables and secrets,** by name (ADR 0022), and the store indexes those uses by environment. A secret can't be part of a longer value, so an app that reads its database from a URL needs a shell in its command, as Umami's does.
- **Dockwell already makes a database and its user on a running server,** as a plan: a `database.user` in that server's environment, with its password a secret there ([ADR 0038](0038-databases.md)). Nothing connects one to an app.
- **1Panel's store does this its own way.** At commit `55061b9`, 49 of its 269 apps use another app's service: a field of their form picks an installed database app, and 1Panel makes a database and a user on it and fills in its host and port. The converter leaves all 49 out. 44 need a SQL database, 13 a Redis, 2 MongoDB, 1 MinIO and 1 Kafka. 16 write the password inside a URL. 30 of them have nothing else that leaves them out.

How others do it:

- **Heroku** attaches an add-on, such as a database, to an app, which sets the app's `DATABASE_URL`. One add-on attaches to several apps, and new credentials reach each.
- **Railway** has reference variables, such as `${{Postgres.DATABASE_URL}}`, between services of a project, on its private network.
- **Render** gives a database an internal and an external URL, and shares values between services through environment groups.
- **Coolify and Dokploy** make a database a resource of its own. An app gets its URL by copying it, and reaches another project's database by joining one network every app shares.
- **1Panel** picks an installed database app, makes the database and user on it, and puts every app on one network, `1panel-network`.

None checks that a database runs a version the app takes. Only Heroku and Railway keep the values live when a password changes.

## Decision

### An entry says what it needs

`Entry.needs` lists what an app reads from outside its own containers. The format is Apache-2.0, as the rest of the entry ([ADR 0012](0012-apache-for-api-schema-and-clients.md)), and an entry without needs installs as before. Each need has:

- **a name,** such as `db`, and a **label,** such as "Database";
- **what meets it,** as `accepts`: engines, each with the versions the app takes, such as MySQL `>=8.0` or MariaDB `>=10.6`. Versions are comparators on dotted numbers, space-separated, as `>=10.6 <12`, and none takes any;
- **the service of its own Compose file that meets it when a person takes a new one,** where the entry brings one, as WordPress's `db`;
- **the database and user** to make for it, such as `wordpress`, where its engine has them;
- **what it sets** in the app's environment, as `sets`: names the Compose file uses as `${NAME}`, each with the part it holds: host, port, address (host and port), database, user, password, or URL. A URL is the engine's own form, `postgres://user:password@host:port/database`, unless a `template` writes another, as `postgres://{user}:{password}@{host}:{port}/{database}?schema=public`;
- whether the app runs without it, as `optional`, which a form shows as a switch.

WordPress's need, for example:

```yaml
needs:
  - name: db
    label: Database
    accepts:
      - engine: ENGINE_MYSQL
        versions: ">=8.0"
      - engine: ENGINE_MARIADB
        versions: ">=10.6"
    service: db
    database: wordpress
    user: wordpress
    sets:
      - { name: DB_ADDRESS, part: PART_ADDRESS }
      - { name: DB_NAME, part: PART_DATABASE }
      - { name: DB_USER, part: PART_USER }
      - { name: MARIADB_PASSWORD, part: PART_PASSWORD }
```

Its Compose file writes `WORDPRESS_DB_HOST: ${DB_ADDRESS}`, and its `db` service makes the database from the same names, `MARIADB_DATABASE: ${DB_NAME}`.

`catalog.Check` refuses a need whose names the Compose file doesn't use, or uses inside a longer value rather than as a whole value or a word of a command or a check, as a secret's are; a name an input has too; a template that names anything but parts; a `service` that isn't one of the entry's databases, runs an engine the need doesn't accept, or doesn't read the need's names; and a companion that depends on a need's service, which isn't there when the need is met otherwise. A part that holds the password, alone or in a template, is a secret; the others are plain values. The password is generated, as a database user's is (ADR 0038), and never asked for.

**When a need is met otherwise,** its own service is left out of the install, with the volumes and inputs only it uses, as a companion turned off is ([ADR 0076](0076-optional-companions-in-a-catalog-entry.md)), and so is every `depends_on` that names it, as the app then waits for nothing of its own.

**The engines** are those Dockwell reads as databases (ADR 0038): PostgreSQL, MySQL, MariaDB, Redis, Valkey, MongoDB and ClickHouse. Each is its own engine, as their versions don't line up: an app that takes MariaDB and MySQL lists both, and one that takes Redis and Valkey lists both, as Dockwell's own entries and the converter will. Percona Server for MySQL is MySQL by what it says of itself, so a connection to one meets a MySQL need; Dockwell runs only the official images as databases. S3-compatible storage and SMTP take the same shape, with parts of their own (an endpoint, a bucket and keys; a sender), and come after the databases.

### Three ways to meet a need

- **A new one beside the app,** in its project, and the default. It's the entry's own service where the entry brings one of the engine chosen, made from the need's values as today: its image makes the database and the user on its first start. For another accepted engine, or an entry that brings none, as 1Panel's don't, it's the catalog's entry for that engine, such as Dockwell's `mysql`, whose database service joins the app's project under the need's name, without its companions. Its image makes the app's database and user on its first start too, from the variables Dockwell knows it reads (ADR 0038: `MYSQL_DATABASE`, `MYSQL_USER` and `MYSQL_PASSWORD`, and their MariaDB and PostgreSQL kin), set to the need's values. It upgrades from that entry, as the pin records.
- **One you run.** A database server Dockwell runs, from its official image, in this project or another the person may change, on the server the app goes on. Dockwell makes a database and a user for the app on it, a `database.user` in that server's environment with its password a secret there (ADR 0038). Redis and Valkey have no users here, so the app takes the server's password and the next logical database no other app on it uses, 0 to 15, and the sheet says, as state, that it can read the other apps' keys.
- **A connection.** A database outside Dockwell, such as a managed one from a cloud provider or another host, that an admin added once (below). It's used as given: its database and its user.

Each ends as a **link**: the need, met.

### A link is the met need, and Variables and secrets shows it

- **A link is a resource,** of kind `link`, in the app's environment, named for the need, such as `db`. Its spec says what meets it (a service, by project, environment and name, or a connection), the engines and versions the app takes, the database and user, and the names it sets with what each holds. It runs on no server: the control plane holds it, as it holds a variable. So meeting a need, and meeting it otherwise later, is a change with a plan, on Activity, and undone like any (PRD principle 2).
- **What it sets are variables and secrets of the app's environment,** one for each name: host, port, address, database and user as variables, the password and a URL that holds it as secrets. The app's services use them by name, as they use any. So Variables and secrets lists them, as the bridge the owner asked for: grouped under the link ("Database · shared-db"), with their source a link to what meets it, and who uses them.
- **They change through the link.** An edit of a value a link sets is refused, naming the link: "DB_ADDRESS is the link db's. Change the link, or detach it." Detaching keeps each value as it is now, as a plain variable or secret, and lets go of the link, as a plan.

### Values that come from somewhere

A variable or a secret may say where its value comes from, as `from`, in place of holding it. Three sources:

- **Another environment's secret or variable,** by project, environment and name. A link to a server in another project takes the password of the user Dockwell made there this way, so the secret lives once, where the user does, and setting it anew there reaches the app.
- **A connection's password,** for a link to a connection.
- **A template of this environment's names,** such as `postgres://${DB_USER}:${DB_PASSWORD}@${DB_ADDRESS}/${DB_NAME}`, which Dockwell writes out with the values it names. A template that names a secret is a secret. This is how a password gets inside a URL with a rotation still reaching it, which ADR 0029 refused for an entry's own file: the whole URL is Dockwell's to write, and it's written again whenever a part changes. Umami's shell can go.

Each is a use, as a service's of a secret is: pinned to the revision of what it names when a change is planned, so a change there takes in the value and every service that uses it, in the same plan, across projects. The store indexes uses with the environment of what they name. A value is read as the agent needs it, at those revisions, and sealed to the servers that run what uses it, as ADR 0022 says of every secret.

### Reaching what meets it

- **In the same project,** by the service's name on the project's network, as today.
- **In another project, on the same server,** through a network made for the link alone, which only the app and the database join, so neither project's other services reach the other. The database answers on it to the need's name, as a new one beside the app would, so what the app reads doesn't change when what meets the need does, and a name the app's own networks have already is refused when it's planned. The database's service joins and leaves such networks **in place, with no restart**, so a server several apps share restarts only for its own changes: the engine sets the networks its links ask for on its spec, as output, and the agent connects or disconnects the running container (`docker network connect`). An agent from before this says nothing of it in its hello, so its plan replaces the container instead, and says so.
- **On another server,** not yet. The choice says where it runs and offers to put the app there. Reaching one through the server's private address, with the firewall open to the app's servers alone, comes later.
- **A connection,** at its host and port, from each server the app runs on, which the check proves.

### Checked before the plan

Each way of meeting a need is checked as the person chooses it, and each check is a line beside the choice:

- **The engine and version.** A running server's version is what it says of itself (ADR 0038's reading), one not read yet its image's tag, a new one the entry's version, and a connection what its check reads. One that doesn't fit is shown, greyed, with why: "Runs MariaDB 10.5; WordPress takes 10.6 or later."
- **Reach** from the server the app goes on, as above.
- **Signing in.** On a server Dockwell runs, the reading signs in as the image's user, which making a user needs. A connection is signed in to through the agent of the app's server: its address, its TLS, its user, its database, and its version, with nothing of what it answers kept but the version and the reason it refused.
- **Names.** The database and user start from the entry's, `wordpress`, or on a server other apps use already, from the app's project's name, so two WordPresses on one server don't clash. A name the server's reading lists is taken, and the next free one stands in (`wordpress_2`).
- **Roles** (below).

The plan checks it all again, and the agent again as it applies, so nothing a check said earlier is trusted later.

### Connections

- **A connection is a resource,** of kind `connection`, at the organization level, as a backup destination is: its engine, host, port, database, user, its password sealed, as a destination's keys are, its TLS (off, required, or verified, with a CA), and the projects that may use it, every one unless it says. It's an admin's to add, change and remove, on Settings, Integrations, under Databases, and from the install sheet, which opens the same form.
- **The form takes a pasted URL,** such as a provider's `postgres://…`, and fills in the parts, the password going straight into the sealed field. It checks the connection from a server as it's filled in, the only server unless there are more.
- **A connection that requires TLS meets a need only where the app can ask for TLS,** through its URL, which carries the engine's own parameter, or a name the need sets for it. Otherwise the check says the app can't, before anything is planned.
- **Removing one is refused while a link uses it,** naming the apps.

### The install sheet

Defaults first, power one step away, as the design system's interaction principles ask:

- **Each need is one row,** as a summary of its default and Change: "Database · New MariaDB 11.4 beside WordPress · Change". The default is new, unless a compatible database on that server already serves another app through a link, which the person has chosen to share, and is then the default. An entry with nothing new to offer, and nothing compatible, shows the need unmet, with Use a connection, and Install waits for it.
- **Change opens the three ways,** each with its picker and its checks: New, with the engine where more than one is accepted; Use one you have, every database server the person may use, compatible ones first, the rest greyed with why; Use a connection, with Add a connection. The database and user names sit under Advanced, as a summary.
- **Picking a server picks where the app goes:** a database on another server offers to put the app there, rather than refusing.
- **The plan says what's created, reused and wired:** "Creates WordPress and a new MariaDB", or "Reuses shared-db: a database and user for WordPress, and a network for the two, joined in place", and "Wires DB_ADDRESS, DB_NAME, DB_USER and the password". The CLI takes the same choice as `--needs db=new`, `db=service:shared-db` or `db=connection:neon-main`.

After it's installed, the app's page has a **Database** card for each need: what meets it, as a link, its engine and version, the database and user, how to connect, its backups, and **Use another database**, which opens the same choice as a plan.

### What each change does

| Change | What happens |
| --- | --- |
| **Upgrade the app** | Its release's needs are checked against its links. One that no longer fits refuses the upgrade, with why and the fix: "shared-db runs MariaDB 10.5; WordPress 7.2 takes 10.6 or later. Update shared-db first." A need a release adds is asked for, as an input a release adds is, new by default. A new database from the catalog upgrades from its own entry. |
| **Upgrade what meets it** | A database server's major move (ADR 0080) is checked against every link that names it, and refused where an app wouldn't take the new release, naming the app. |
| **Undo** | As any change. Undoing the change that made a database and user on a server drops them, which destroys data, so only a person applies it, as ADR 0038 says. |
| **Delete the app** | Its links and their values go with it. A new database beside it goes with the project, as today ([ADR 0074](0074-projects-you-can-see-and-that-go-with-their-last-app.md)). On a server it shared, its database and user stay, as they live in that server's environment: the server's Users tab marks them "No app uses it", with Delete, and the plan says so in one line. |
| **Delete what meets it** | Refused while a link names it, a project's deletion with what's in it too, naming the apps that use it, with a way to point each elsewhere. A shared service never goes with an app. |
| **Backups** | A database's data is in its server's volume, so it's backed up with its server (ADR 0033). The app's page shows those backups and backs the server up on demand. An update of the app backs up the databases it links first, a dump that stops nothing, as ADR 0080 backs up its volumes. A restore of a server names every app that links to it. |
| **Roles** | Using a server in another project needs a role there that may change it (a developer, an admin where it's protected), as a user is made there. A link that takes that server's own password, as a Redis link does, needs what reveals its secrets too: a person, not a token ([ADR 0049](0049-roles-and-invites-by-project.md)). The choice lists only what the person may use. A connection is an admin's to add, and anyone's who may change a project it allows to use. |
| **Rotate a password** | Setting a user's password anew (ADR 0038's `ChangeDatabasePassword`) restarts every app that uses it through a link, in the same plan, with each URL written again. |
| **Use another database** | A plan: a database and user on the new one, the values set again, the network swapped, the app restarted. Its data doesn't move, which the plan says in a line; the old database stays where it was, marked "No app uses it". |
| **Move the app or its database** | A change that would leave a link's two ends on different servers is refused, naming the link, unless it moves both. A connection is checked again from the new server. |

### Other stores

The converter maps a store's "another app's database" to needs, which is Track K's (#570), with the shape this record gives. 1Panel's field of type `apps` with its `values` and a `service` child, or a field of type `service` with its `key`, is a need, with the engines they name and no versions. The names 1Panel fills in or asks for, `PANEL_DB_HOST`, `PANEL_DB_PORT`, `PANEL_DB_NAME`, `PANEL_DB_USER`, `PANEL_DB_USER_PASSWORD` and their Redis kin, are its parts. Its password is never asked for: Dockwell makes one, or a connection gives one. A URL that holds them, as docmost's `DATABASE_URL`, is a URL with its template. A field that asks for a server's administrator, as mongo-express's does, is no need: such a tool administers another app's database, and stays out with that reason. Dokploy's and Coolify's templates bring their databases, and may mark them as needs with their service, so they can use one you run too.

### What's installed already

- **Entries:** Dockwell's own entries that bring a database are rewritten with needs, in Dockwell/catalog, each a new release. Each names its password as its secret input was named, such as `MARIADB_PASSWORD`, so an install's secret, and what its database signed its user in with, stay as they are. An entry without needs installs as before.
- **Installs:** an install moves to a link at its next upgrade to such a release, in the same plan. The upgrade's three-way merge reads the database and user it has, keeps its secret, and records how each need is met in the pin (`CatalogPin.needs`). Until then it runs as it does, and its page offers to update before choosing another database.
- **Agents:** joining a network in place needs an agent that says so; an older one restarts the database instead, which its plan says.

### What the agent build links

Joining and leaving a network in place, in the container driver, and the connection's check: a small client for each engine's sign-in (PostgreSQL's SCRAM-SHA-256 and md5, MySQL's `caching_sha2_password` and `mysql_native_password`, MariaDB's, Redis's `AUTH`) that reads the version and nothing else. No driver library. `make budgets` measures it against the agent's 40 MB.

### Numbers

`Entry.needs` 40 and new messages in `catalog.proto`; the Service spec 190 to 199, for the networks its links ask for; `from` 80 in `Variable` and `Secret`; `CatalogPin.needs` 82; 80 to 89 in other messages of the API; and 290 to 299 in `agent.proto`, for the hello's word and the connection's check. Migrations take main's next number when they land.

## What we turned down

- **One network for every app,** as 1Panel's, Coolify's and Dokploy's. Any app would reach every database.
- **Copying the values once,** as Coolify, Dokploy and a copied URL do. A new password, or a database moved, breaks the app with nothing to say why.
- **Joining the two projects' networks.** Every service of each would reach the other's.
- **A database as a kind of its own,** as an add-on. ADR 0038 keeps a database a service, with everything a service has.
- **References from any value to any other,** as Railway's. A link carries exactly what a need takes, with the roles a database user needs. `from` leaves room for more when something needs it.
- **Making a database on a connection's server.** It isn't Dockwell's to change. A connection is used as given until a need asks for more.
- **A database on another server, now.** It needs a firewall rule for the app's servers and a check that holds through a server's address changing, which is a slice of its own.
- **Guessing needs from any Compose file by its variables' names.** A wrong guess wires the wrong database. For an app from no entry, a link is a person's choice, on its Environment tab, as Connect a database.

## Consequences

- A person chooses an app's database as they install it, checked before the plan, and changes it later as a plan.
- One database server can serve several apps, each with its own database and user, and none of them reaches the others' services.
- 1Panel's apps that use another app's database can convert, and 30 of the 49 have nothing else in the way.
- A password inside a URL is Dockwell's to write, so it follows a rotation.
- The store indexes uses and references across environments, which is a migration, and the revision engine follows a chain of uses (a secret, a URL that names it, a service that uses the URL) until nothing more joins a change.
- Deleting a shared database is refused while apps use it, and deleting an app leaves its database on a shared server, marked, one click from gone.
- The agent build gains a little, which `make budgets` reports.

## Order of work

1. This record.
2. The format and its check, which Track K converts to; the link; an install whose needs are met new beside the app, by the entry's own service or the catalog's entry for another accepted engine; the install sheet's row for each need. A URL that holds a password waits for 3.
3. One you run: values from a template and from another environment, uses and references across environments, a database and user made on a server in another project on the same server, the link's network joined in place, its checks, roles, and the refusals on delete and move. Use another database on an app's page.
4. Connections: their resource, Settings, the agent's check, and Use a connection.
5. Dockwell's own entries with needs, in Dockwell/catalog once the check it runs knows them, installs moving at their upgrade, Connect a database on any app's Environment tab, and Variables and secrets grouped by link, with Track V's work on an app's own values (#565).
