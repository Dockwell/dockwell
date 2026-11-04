-- SPDX-License-Identifier: AGPL-3.0-only
-- The object model: the organization, scopes beyond a server, and
-- deployments. See docs/adr/0014-object-model-and-store.md.
--
-- A resource now lives in a scope, named by ID: the organization (its
-- projects), a project (its environments), an environment (its services),
-- or a server (host-level kinds such as firewall rules). The kind says
-- which. A deployment is a revision placed on a server, with its rollout
-- state.
--
-- The M1 tables are rebuilt with scopes. Only tests ever wrote them, but
-- their rows are copied all the same.

-- The identity, billing and policy boundary. An install has one.
CREATE TABLE organizations (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE m1_resources AS SELECT * FROM resources;
CREATE TABLE m1_revisions AS SELECT * FROM revisions;
CREATE TABLE m1_change_items AS SELECT * FROM change_items;
DROP TABLE revisions;
DROP TABLE change_items;
DROP TABLE resources;

-- Every resource Dockwell has managed, deleted ones included, so a name
-- that comes back continues its history.
CREATE TABLE resources (
  id       TEXT PRIMARY KEY,
  kind     TEXT NOT NULL,
  scope    TEXT NOT NULL,    -- the ID of the organization, project, environment or server it lives in
  name     TEXT NOT NULL,    -- unique among resources of its kind in its scope
  revision INTEGER NOT NULL, -- the latest revision's number
  UNIQUE (kind, scope, name)
);

-- Each revision of a resource's spec, numbered from 1. Never updated.
CREATE TABLE revisions (
  resource_id TEXT NOT NULL REFERENCES resources (id),
  number      INTEGER NOT NULL,
  spec        BLOB,             -- NULL: deleted
  change_id   TEXT NOT NULL REFERENCES changes (id),
  position    INTEGER NOT NULL, -- of the change item that made it
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (resource_id, number)
);

-- What a change asks for, resource by resource.
CREATE TABLE change_items (
  change_id     TEXT NOT NULL REFERENCES changes (id),
  position      INTEGER NOT NULL,
  resource_id   TEXT NOT NULL,            -- given when the change is planned, so another item can live in a new one
  kind          TEXT NOT NULL,
  scope         TEXT NOT NULL,
  name          TEXT NOT NULL,
  base_revision INTEGER NOT NULL,         -- the revision the edit starts from; 0 for none
  spec          BLOB,                     -- NULL deletes
  servers       TEXT NOT NULL DEFAULT '', -- where it is placed, by server ID, separated by spaces
  PRIMARY KEY (change_id, position)
);

-- A revision placed on a server. The revision, the server and when are
-- fixed; the rollout state moves on: pending until the server has it, then
-- live or failed, and superseded once a later revision, or a deletion,
-- takes its place there.
CREATE TABLE deployments (
  resource_id TEXT NOT NULL,
  revision    INTEGER NOT NULL,
  server_id   TEXT NOT NULL REFERENCES servers (id),
  state       TEXT NOT NULL CHECK (state IN ('pending', 'live', 'failed', 'superseded')),
  error       TEXT,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (resource_id, revision, server_id),
  FOREIGN KEY (resource_id, revision) REFERENCES revisions (resource_id, number)
);

INSERT INTO resources (id, kind, scope, name, revision)
  SELECT id, kind, server_id, name, revision FROM m1_resources;
INSERT INTO revisions (resource_id, number, spec, change_id, position, created_at)
  SELECT v.resource_id, v.number, v.spec, v.change_id,
    COALESCE((SELECT MIN(ci.position) FROM m1_change_items ci JOIN m1_resources r ON r.id = v.resource_id
      WHERE ci.change_id = v.change_id AND ci.kind = r.kind AND ci.server_id = r.server_id AND ci.name = r.name), 0),
    v.created_at
  FROM m1_revisions v;
-- M1 gave a resource its ID when it was applied, so an item that made one
-- has its ID, and one still planned has none.
INSERT INTO change_items (change_id, position, resource_id, kind, scope, name, base_revision, spec, servers)
  SELECT ci.change_id, ci.position,
    COALESCE((SELECT r.id FROM m1_resources r WHERE r.kind = ci.kind AND r.server_id = ci.server_id AND r.name = ci.name), ''),
    ci.kind, ci.server_id, ci.name, ci.base_revision, ci.spec, ci.server_id
  FROM m1_change_items ci;
-- In M1 a resource was on its server; the latest revision of each one that
-- isn't deleted is what runs there. Its change says how that went: applied
-- is live, failed is failed, and applying is still pending. A change only
-- has revisions once it is applying, so none is planned.
INSERT INTO deployments (resource_id, revision, server_id, state, created_at, updated_at)
  SELECT r.id, r.revision, r.server_id,
    CASE c.state WHEN 'applied' THEN 'live' WHEN 'failed' THEN 'failed' ELSE 'pending' END,
    v.created_at, v.created_at
  FROM m1_resources r
  JOIN m1_revisions v ON v.resource_id = r.id AND v.number = r.revision
  JOIN changes c ON c.id = v.change_id
  WHERE v.spec IS NOT NULL;

DROP TABLE m1_change_items;
DROP TABLE m1_revisions;
DROP TABLE m1_resources;

CREATE INDEX resources_scope ON resources (scope, kind, name);
CREATE INDEX revisions_change ON revisions (change_id, position);
CREATE INDEX deployments_server ON deployments (server_id);
