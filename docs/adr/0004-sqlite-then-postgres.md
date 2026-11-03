# 0004. SQLite by default, Postgres for high availability

- Status: Accepted
- Date: 2026-11-03

## Context

The PRD's scaling modes put SQLite on a single box and in fleets of up to about 250 servers, and Postgres under the three-replica, highly available control plane and in federated regions. Moving up a mode must not need a migration of the product, only of its store.

## Decision

- The control plane talks to its state through one store interface. Implementations exist for SQLite and Postgres.
- SQLite uses a pure-Go driver (`modernc.org/sqlite`) in WAL mode, so the binary stays static.
- Schema migrations are written for both dialects and run against both in CI from the first migration.
- Queries stay portable SQL. Anything dialect-specific sits behind the interface, with a test.

## Consequences

- Single-server installs need nothing beyond the binary, and backing up the control plane is a consistent SQLite snapshot (`VACUUM INTO`). That snapshot is what gets copied to other agents and off-site.
- Testing against two databases from day one costs a little CI time now and avoids a painful port later.
- Some conveniences (`LISTEN/NOTIFY`, advisory locks) can't be relied on in the default mode. Events and locking are designed to work on SQLite first.
