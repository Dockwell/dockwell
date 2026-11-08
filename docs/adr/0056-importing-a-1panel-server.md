# 0056. Importing a 1Panel server

- Status: Accepted
- Date: 2026-11-08

## Context

Issue #262 asks for an importer for 1Panel, a launch requirement in the PRD ("Importers for 1Panel and plain Compose", Server, P0), behind its signature experience "Move in, don't start over". The owner runs 1Panel on two servers and means to move them to Dockwell.

What exists already:
- [ADR 0031](0031-containers-dockwell-didnt-make.md) takes on a running container or a whole Compose project with no restart, and says what 1Panel does to an app taken on that way.
- [ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md) installs apps from a catalog, and #302 converts 1Panel's app store into catalog entries.
- In review: websites (#206, ADR 0039), databases (#207, ADR 0038), scheduled tasks (#259) and the firewall (#288, ADR 0052).

Nothing reads what 1Panel itself knows: which store entry and version an install is, its websites and their PHP versions, its runtimes, databases and their users, its cron jobs and certificates. This record says how the importer reads that, what it does with each thing, and what it leaves, with a reason a person can act on.

It was written from 1Panel's own source, never a live server:
- v2.4.2, commit `832d367428`, both its `agent` and `core`;
- v1's last line, `release-1.10` at `20b89a8fdd`;
- its app store at `fb78ceed0c`, for how OpenResty runs.

## Decision

### Where 1Panel keeps what it knows

1Panel is installed under a base folder, `/opt` unless the person chose another, which `/usr/local/bin/1pctl` names as `BASE_DIR=`. Everything is under `<base>/1panel`:

| What | v2 (2.0 on) | v1 (1.10 LTS) |
| --- | --- | --- |
| Its records | SQLite, `db/agent.db`, with `db/core.db` for the panel's own settings | SQLite, `db/1Panel.db` |
| Its version | `settings.SystemVersion` in `agent.db` | `settings.SystemVersion` |
| Store apps | `apps/<app key>/<install name>/`, holding `docker-compose.yml`, `.env` and the folders it binds; `apps/local/<key>/<name>/` for a local app | The same |
| Runtimes | `runtime/<type>/<name>/`, a Compose project each | The same |
| Websites | `<WEBSITE_DIR>/conf.d/<alias>.conf` and `<WEBSITE_DIR>/sites/<alias>/`, with the files to serve in `index/`. `WEBSITE_DIR` is a setting, `<base>/1panel/www` unless set | `apps/openresty/<install>/conf/conf.d/<alias>.conf` and `apps/openresty/<install>/www/sites/<alias>/` |

Both write SQLite in WAL mode, so what was written lately may be in `agent.db-wal` and not yet in `agent.db`.

The tables the importer reads, by the names GORM gives them:

| Table | What a row is | What the importer uses |
| --- | --- | --- |
| `settings` | A key and a value | `SystemVersion`, `WEBSITE_DIR` |
| `apps`, `app_installs` | A store app, and an install of it | key, name, version, status, container name, ports, which store it came from (remote, local or custom) |
| `runtimes` | PHP, Node, Java, Go, Python or .NET, each a Compose project | name, type, version, status, image, container name, its code folder |
| `websites`, `website_domains` | A site, and the domains and ports it answers | alias, type, primary domain, domains, its folder, what it proxies to, its runtime, its certificate, whether it has rewrite rules |
| `website_ssls` | A certificate | its domains, how it was got (ACME, DNS, manual, self-signed), when it ends. Its key and chain are there in plain PEM: the importer never reads those columns |
| `databases` | A database server, local or remote | type, version, local or remote, which install it is |
| `database_mysqls`, `database_postgresqls`, and v2's `database_users` | A database on a server, and its users | names, which server, format, permission. Passwords are AES-encrypted with a key in the same database (`settings.EncryptKey`): the importer never reads those columns |
| `cronjobs` | A job 1Panel's own scheduler runs | name, type, schedule, status, what it acts on. Not the script, nor a URL's path and query, which can hold a password |

### How it reads: on the server, by its agent, read-only

1Panel's records are on the server it runs on, owned by root, so the agent reads them, as it reads files ([ADR 0032](0032-files-on-a-server.md)). One call, `ReadOnePanel`, returns what was found and nothing secret. The control plane decides what Dockwell would do with each.

- **The agent build links no SQLite.** `modernc.org/sqlite` and `database/sql` are the control plane's alone ([ADR 0023](0023-a-full-build-and-an-agent-build.md)), and shipping 1Panel's databases to the control plane would send its encrypted passwords with the key to them. So the agent reads the file format itself, read-only, with a small reader of its own (`internal/agent/sqlitefile`): table pages and overflow pages, the records in them, and the WAL's committed frames over them. It needs no index, as a table here is read whole. Columns are read by name, from the table's own `CREATE TABLE`, so a column 1Panel adds later is ignored, and one it hasn't yet reads as empty.
- **Nothing is written, locked or opened for writing.** The files are opened read-only, without following a link, and the WAL is read as it stands; 1Panel goes on as it was. A read that sees the file change underneath it reads again, up to three times, then says it couldn't get a steady read.
- **What it sends is what a plan shows.** Names, versions, domains, paths, types, schedules, and whether a site has rewrite rules or a job a script. Never a password, a key, an app's environment, a script, a command or a URL's path. The values the import needs to take something on travel later, sealed, with the plan that takes it on, as an app's environment does in ADR 0031.
- **Versions.** It reads 1Panel v2, from 2.0 to 2.4.2, the layout read here, and v1.10 LTS. Another version is read the same way, by column names, and the answer says it wasn't one read for this record.

### What it does with each thing

Every import is a plan a person reviews and applies, and nothing on the server changes before that. `ImportService.ImportOnePanel` returns all of this, and `dockwell import 1panel --server-id <server> --plan-only` lists it. It records nothing yet: once it plans the import, it returns that change too, and `--plan-only` leaves it planned, as it does for every plan.

**Taken on in place, with no restart** (ADR 0031):

- **Store apps** become Compose projects, each with what it binds where it is, by the project's label. The plan names the store entry and version each came from, and the catalog entry that matches, where the catalog has one.
  - They aren't made catalog installs, pinned to an entry, yet. An entry #302 converts runs on named volumes and loopback ports, and an install of 1Panel's binds folders in its own folder. An upgrade from the entry would plan new, empty volumes, which is the worst thing this could do. A move from a taken-on project to its entry, with its data, is a later step of its own.
- **Runtimes other than PHP,** a Node or Python app and the like, are Compose projects too, and come the same way.
- **Database servers** are store apps, so they come as above, with their data where it is. The databases and users in them come with Dockwell's databases (#207): the plan lists them, and a user's password becomes a sealed secret as it's taken on, never shown.

**Made again in Dockwell, as the site it was:**

- **Websites** become Dockwell websites (ADR 0039), in the place they had:

  | 1Panel's site | Becomes |
  | --- | --- |
  | Static | A website serving the same folder, `sites/<alias>/index`, in place |
  | PHP (a runtime) | A PHP website on that folder, at the PHP version Dockwell has that's closest without going back a minor. PHP under 8.2 stays, as Dockwell runs 8.2 to 8.5 |
  | A reverse proxy | A website sending to the same URL, or to the same port on the server |
  | An app's site (`deployment`) | A route to the taken-on app's service and port |
  | A subsite, or a TCP or UDP stream | Stays: Dockwell has neither |

  What a website can't say yet is listed with each, so nothing goes quietly. That covers rewrite rules, redirects, basic auth, another domain beyond the first, a site's own Nginx lines and 1Panel's WAF.
- **Certificates aren't moved.** Dockwell's ingress gets its own for each website's domain when it takes over ports 80 and 443. 1Panel's stay in its records. A certificate a person uploaded, or a self-signed one, says that Dockwell would get one from Let's Encrypt in its place, and that a domain which can't get one goes behind a tunnel or a proxy that ends TLS (ADR 0030).
- **Cron jobs** become scheduled tasks (#259): a shell job, a URL to call, and a backup of an app, a site, a database or a folder. A job that runs in a container says which, and the plan shows its schedule in words. 1Panel's own housekeeping stays with 1Panel, each with why:
  - the clock (`ntp`), which the server's own time service keeps;
  - cutting site logs (`cutWebsiteLog`), which Docker rotates for Dockwell's websites;
  - cleaning 1Panel's caches and logs (`clean`, `cleanLog`, `log`);
  - snapshots of 1Panel itself;
  - syncing 1Panel's IP groups.

**Left as it is, with the reason:**

- **OpenResty** holds ports 80 and 443 on the host's network (`network_mode: host` in 1Panel's store), and Dockwell's ingress needs them. It stays 1Panel's until a person hands those ports over. That's a step of its own, after the websites are planned:
  1. Stop OpenResty in 1Panel.
  2. Apply the websites' plan.
  3. Check each site.
  4. Undoing it is deleting the websites and starting OpenResty again.

  The sites are down from the stop until the ingress has its certificates, a minute or so, and the plan says so.
- **The firewall** is the server's own, ufw or firewalld, which 1Panel writes to directly. Dockwell's Firewall tab reads it where it is (ADR 0052), so there's nothing to import.
- **A remote database** is on another machine: the plan says to add it from Dockwell's Databases.
- **1Panel itself, its settings, users, backup accounts and snapshots** aren't Dockwell's to take: its login stays its own, and it goes on running until a person uninstalls it.

### A way back

What's taken on is let go of by deleting it before anything replaces it, which leaves 1Panel's app as it was (ADR 0031). A website made again is deleted with its plan's undo. Starting OpenResty in 1Panel then serves the site as before, as nothing of 1Panel's was changed. The importer never deletes, stops or edits anything of 1Panel's: stopping OpenResty is the person's, in 1Panel.

### The screen

A server running 1Panel says so on its page, with what an import would take on, make again and leave, from the same read, and the plan to start. It's built from the system's components, as no mock has it, and it comes with the plan that takes things on.

### Numbers

The agent protocol takes 180 to 189 for this: `Hello.reads_onepanel`, and `read_onepanel` in `Call` and `CallResult`.

## Consequences

- **A move is two plans, and a step between.** The apps come with no downtime. The websites come when a person hands over 80 and 443, with a minute down, which they choose when.
- **Nothing secret leaves the server to be read.** A plan-only import reads no password and no key, and what it finds is what a plan would show. So a read-write token can ask for one, as it can for `CheckCompose`. The import itself takes a person, as taking a container on does (ADR 0031).
- **The agent reads a file format.** Its SQLite reader is small and read-only, and it's tested against databases SQLite itself writes, in WAL mode and not. A file it can't read is refused with why, never guessed at.
- **What 1Panel adds next isn't known.** Columns are read by name, so most of what a new version adds passes by. A table renamed or split would read as empty, and a test against each version read here says so.
- **Catalog installs come later.** A store app comes as a Compose project, named with its entry, until a move to the entry can carry its data.
- **Websites, databases, scheduled tasks and the firewall** come with their own features. Until each is on the control plane, the plan lists that part and names the issue it waits for.
