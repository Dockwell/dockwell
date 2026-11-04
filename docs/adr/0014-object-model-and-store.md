# 0014. The object model, and one store on SQLite and Postgres

- Status: Accepted
- Date: 2026-11-04

## Context

The PRD's object model has organizations, projects, environments, services, revisions, deployments and servers (nodes). Three principles shape how they fit:
- **Principle 1:** one model, every surface. The panel, API, CLI and MCP server see the same objects.
- **Principle 2:** every change is a revision with a plan, reversible in one action.
- **Principle 4:** a single-server user never sees projects, environments or pools until they have a choice. The PRD's table for scaling from one server to a fleet says how: a hidden default project, a hidden production environment, and no server choice with one server.

[ADR 0009](0009-resources-revisions-and-plans.md) made every managed thing a resource: a kind, a place, a name and a spec, with immutable revisions and changes that are planned and applied. Its place was a server, and it left project and environment scopes to M2, with services.

[ADR 0004](0004-sqlite-then-postgres.md) put SQLite under single servers and fleets, and Postgres under the highly available control plane:
- one store interface, with an implementation for each;
- migrations for both, run against both in CI from the first;
- portable queries, with anything dialect-specific behind the interface.

Until now only SQLite existed. Issue #19 asks for the object model in the store, on both databases, with revisions immutable and every write through the revision store. Issues #20 to #23 build on it: the plan engine's API, the event timeline, the CLI and REST.

## Decision

### The objects

| Object | In the store | Written by |
| --- | --- | --- |
| Organization | A row in `organizations`. An install has one, made on its first start. | `revision.Setup` |
| Project | A resource of kind `project`, in the organization | Changes |
| Environment | A resource of kind `environment`, in a project | Changes |
| Service | A resource of kind `service`, in an environment | Changes |
| Revision | A row in `revisions`: an immutable snapshot of one resource's spec | Applying a change |
| Deployment | A row in `deployments`: one revision placed on one server, with its rollout state | Applying a change |
| Server | A row in `servers`, as in M1 | Joining, and the agent stream |

A server is identity, not managed state: it joins with a ticket and reports over its stream, as [ADR 0007](0007-one-binary-two-roles.md) and the [server identity threat model](../security/server-identity.md) describe. So is a person. Everything else on the list is managed state.

### Every resource lives in a scope

Each kind has a level, which says what its resources live in. `resources.scope` holds that thing's ID, and a name is unique among its kind in its scope.

| Level | A resource lives in | Applies on | Kinds now | Later |
| --- | --- | --- | --- | --- |
| Server | A server (`srv_…`) | That server | `container`, `firewall.rule` | Packages, files, systemd units |
| Environment | An environment (`env_…`) | The servers it is placed on | `service` | Domains, routes, secrets, volumes |
| Project | A project (`prj_…`) | The control plane alone | `environment` | Members, project secrets |
| Organization | The organization (`org_…`) | The control plane alone | `project` | Policies |

- **Projects and environments are kinds, not tables of their own.** Making, renaming or deleting an environment is a change with a plan, shows in the timeline, and undoes in one action, as principle 2 asks. #22's rule that every mutating command shows a plan then holds without exception. Their plans have revisions and no steps, like adopting drift.
- **The organization is a table.** It is the identity, billing and policy boundary, and one per install until federation. Its policies will be kinds at the organization level.
- **Today's server-scoped kinds stay host-level.** The PRD keeps machine-level things, such as files, the firewall and packages, on the server's own page, and a container run by hand belongs there too. An app in an environment is a service.
- **For now, a service is an app from an image.** The agent runs it as one container on each server it is placed on, named `<name>.<environment ID>`, such as `web.env_01K…`. Two environments' services of one name can then share a server. A container run by hand can't take a name with `.env_` in it, so neither ever manages the other's. #26 replaces this runtime with Docker and Compose, behind the same kind.
- **The scope has no foreign key**, because it may be in one of three tables. The revision store checks it when a change is proposed, and again in the transaction that applies it: the scope must exist, be live, and be what the kind's level lives in.
- **A change is checked as a whole:** what is stored, plus what the change makes and deletes. A scope the change makes is there for what it makes inside it, and one it deletes is gone. So one change can bring back a project with its environments, as undoing its deletion does, and no change can delete an environment while it makes or edits a service in it.
- **A scope goes only once it is empty.** What lives in it is deleted first, or in the same change. A project keeps at least one environment, and the organization at least one project, so "the only one" always exists. A successor made in the same change counts, so the last one can be replaced in one change.
- **A project never exists without an environment.** A resource gets its ID when its change is proposed, not when it applies, so another item in the change can live in it. A change that brings a project to life also makes its first environment: `production`, as the install's first project has, unless the change makes one in it. A project made again continues its history, and so does its `production`. Undo and rollback keep the rule: undoing a project's creation deletes its environment with it, and undoing its deletion brings back the environments deleted with it. The other way was to require the first environment in the same change. That would make every caller name one, while principle 4 asks that a new project look like the default one until there's a choice.
- **Names of projects, environments and services are DNS labels**: 1 to 63 lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit, such as `api-v2`. They go in URLs, in a service's hostname and container name, and on the command line, so a name that would fail on a server is refused when it is proposed. A spec's `display_name` is what the panel shows.

### Placement and deployments

A deployment is a revision placed on a server.
- A host-level resource is deployed on its server.
- An environment-level one is deployed on the servers its change places it on.
- Projects and environments are deployed nowhere.

**Placement isn't in the spec.** The spec is what runs, and the deployments are where, as the PRD's revision and deployment rows say. Placement is still versioned: moving a service writes a new revision with the same spec and new deployments, so a move has a plan and undoes like any edit. The plan runs the new copy before the old one goes, as ADR 0009 already orders creates before deletes.

**Placement has a hidden default too.**
- An edit that names no servers keeps a service where it runs.
- A new service runs on the only server.
- Once there are two, a new service must name where it runs, which is the CLI's `--on`.

**A removed server takes no steps.** Once a server is revoked, nothing runs there, so the revision store treats it as gone:
- Moving a service off it, or deleting a resource from it, plans no step there. The plan lists it as removed, and its deployments there are superseded at once. Waiting for the server instead would wait forever, since a removed server never reconnects.
- Nothing is placed on it. Naming it is refused, and an edit that names no servers keeps a service on its servers still in the fleet. One that ran only on removed servers is placed like a new one.
- Undo and rollback follow the same rules. They put a spec back on the servers still in the fleet, and are refused when none of them is. A rollback, or a change left applying, stops waiting for a server once it is removed.

**A deployment's revision and server never change; its rollout state moves on.**
- *Pending* when the change commits.
- *Live* or *failed* as its step completes. A deployment with nothing to do is live at once, and one whose server is removed before it gets there is failed.
- *Superseded* once a later revision is live on that server, or a deletion takes the resource off it.

Health comes with #28. Drift, and later the agent's cache of desired state (#32), read the latest revision deployed on each server.

### Hidden defaults, and when there is a choice

On its first start, and on the first start after an upgrade, the control plane makes an organization named `default`. Its project is `default`, and that project's environment `production`. All three are made in one transaction, the project and environment as a change by `dockwell`, applied at once.

"Until there's a choice" means this, in `scope.Tree.Shown`:
- **Organization:** shown once a person can reach two organizations, so not before federation.
- **Project:** shown once the organization has two live projects.
- **Environment:** shown in a project once it has two live environments. A view across projects shows it once any project has two.
- Deleted ones don't count, and nor do names: a renamed default is still the only one. A level hides again when it is back to one.

Requests follow the same rule, in `scope.Tree.Resolve`:
- A level left out means the only one there is.
- Once there is a choice, leaving it out is an error that names the options (`scope.ChoiceError`), for the panel or CLI to ask which.
- Naming one always works, by name or ID. An environment's ID needs no project.

The API always returns scope IDs, because principle 1 makes the API the whole model; surfaces use `Shown` to decide what to draw. The timeline (#21) hides changes that touch only hidden objects, such as the one that made the defaults.

### Revisions are immutable, and only the revision store writes

- **In Go.** The writes to managed state are methods of `store.RevisionTx`: changes, revisions, deployments, the scopes' resources, and the organization. Only `store.DB.Revise` hands one out, and `store.Tx` has none of them. A test parses every Go file in the module and fails when a package other than `internal/revision` uses either.
- **In the database.** Triggers, on both databases, refuse:
  - any update or delete of a revision or a change's items;
  - an update of what a change asked for: its summary, actor, plan, fingerprint and what it reverts;
  - a resource moving anywhere but to its next revision, or changing kind, scope or name;
  - a deployment changing revision or server;
  - deleting a change, a resource or a deployment.

  Only a change's state and a deployment's state move on.

### One store, two databases

**The driver is pgx**, v5.11.0 through `database/sql`, so both databases share one code path. There is no ORM or query builder: the queries are short and few, and both databases speak them.

**Queries are shared.** They are written for SQLite, with `?` and `?NNN` placeholders, which the Postgres dialect rewrites to `$n`, skipping quoted text and comments. Two things a query must do to mean the same on both, which the tests on Postgres found:
- qualify column names in `ON CONFLICT … DO UPDATE`;
- `CAST` a reused parameter whose type Postgres can't deduce.

**Migrations come from one source.**
- Each version is one portable file. Its column types `INTEGER`, `BLOB` and `TEXT` become `BIGINT`, `BYTEA` and `TEXT COLLATE "C"` on Postgres, and blob literals become bytea ones.
- The "C" collation compares bytes, as SQLite does, so `ORDER BY` and `UNIQUE` agree on both.
- What can't be said once goes in a pair of files, `NNNN_name.sqlite.sql` and `NNNN_name.postgres.sql`, run after the portable part. The loader refuses one without the other. Migration 0006's triggers are the first pair.
- Migration 0006 also rebuilds M1's revision tables with scopes. Only tests ever wrote them, but it copies their rows. The latest revision of each M1 resource becomes a deployment on its server, live if its change applied, failed if it failed, and pending if it was still applying.

**Transactions keep SQLite's semantics on both.**
- **Writes take turns.** On SQLite, one writing connection begins `IMMEDIATE`. On Postgres, each process also has one writing connection, and every write begins with `pg_advisory_xact_lock` on a key for the schema, so writers in other processes and replicas queue too. Writes run at `READ COMMITTED`, so each statement sees what the writer before committed. With the lock taken first, that is what `IMMEDIATE` gives.
- **Reads see one snapshot.** SQLite's deferred readers in WAL mode get one from their first read. Postgres reads run at `REPEATABLE READ READ ONLY`, which gives the same, and being read-only they never fail to serialize.
- **A conflict leaves the transaction usable.** Postgres aborts a transaction at its first failed statement; SQLite undoes only that statement. Inserts that may break a unique constraint, such as a server's name, run under a savepoint on Postgres, so a caller that gets `ErrConflict` can try another name in the same transaction.

ADR 0004 says not to rely on advisory locks in the default mode. Nothing does: the lock makes Postgres behave like SQLite, and the design still works on SQLite first. One writer at a time caps how fast writes go. Writes are changes, joins, sign-ins and reports, so that cap is far off at 250 to 2,000 servers. If the highly available control plane measures writes queueing, row locks or `SERIALIZABLE` with retries can replace the lock behind the same API.

**Configuration.** `dockwell run --database`, or `DOCKWELL_DATABASE`, takes a `postgres://` URL. Unset, the state stays in SQLite in the data directory. Commands beside the control plane take the same setting.
- **The password stays secret.** Messages and logs show the URL through `store.Redact`. The flag's default isn't the variable, so help never prints it, and errors name the setting, not its value. The install guide recommends `.pgpass` or a root-only `EnvironmentFile`, since a unit file can be read by anyone on the server.
- **A second install never starts by accident.** `dockwell run` refuses Postgres when the data directory already has SQLite state, since moving between them isn't supported yet. Once the data directory's key belongs to an install, it also refuses to start a new one on an empty database or without its `state.db`. The control role now follows the key, which every control plane has, not `state.db`.
- **The first start is all or nothing.** Its key waits as `control/key.new` until the install and its defaults are on record, then takes its place as `control/key`. So a first start that fails, such as with Postgres out of reach, leaves no key that claims an install, and the next start carries on with the key it left. A start that stopped after the install was on record but before the move finds that install and finishes it.

**Tests run on both.**
- `storetest.Each` runs a test once on each database: the store's, the revision store's, sign-in, identity, the control server's sign-in, and `dockwell run`'s.
- Postgres needs `DOCKWELL_TEST_POSTGRES`. Without it, those runs skip and say how to turn them on. Each test gets a schema of its own.
- CI's Go job runs Postgres 18 as a service container. This change was also run on Postgres 17. The SQL needs Postgres 11 or newer.

## Consequences

- **#20 builds on the engine as it stands:** `Propose`, `Apply`, `Undo` and the per-server plan. Deployments are what apply's progress reports on, and `Edit.Servers` is where a request places a service. A plan's servers can include removed ones, with no steps, for the plan sheet to show as removed. `scope.Tree.Resolve` fills in a request's scope, and a `ChoiceError` becomes the CLI's prompt.
- **Every new kind declares its level.** Machine-level kinds are host-level; anything that belongs to an app is environment-level, placed on servers, and gets deployments.
- **Projects and environments need no API of their own to follow the model.** Their API is the resource API that #20 and #23 expose, with `Shown` and `Resolve` beside it.
- **Moving an install from SQLite to Postgres needs a copy tool.** ADR 0004 promises it as "a migration of its store", not of the product. Until it exists, `dockwell run` refuses the switch.
- **Each piece of SQL now has two databases to satisfy.** CI catches what doesn't. Something only one of them can express costs a pair of dialect files.
- **The binary grows with pgx,** by about 6 MB. Its package setup runs in every process, the agent's too, so an idle agent uses about 1.6 MB more, still well within its 40 MB budget.
- **Service containers are named by a prototype rule.** #26 replaces it with Compose's labels, so plain `docker compose` sees them, and the `.env_` names kept for services are free again.
