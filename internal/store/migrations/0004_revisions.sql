-- SPDX-License-Identifier: AGPL-3.0-only
-- The revision store: every change, the resources it touched, and each
-- immutable revision of their specs. See
-- docs/adr/0009-resources-revisions-and-plans.md.

-- A change is one action's worth of edits: planned, then applied or failed.
CREATE TABLE changes (
  id          TEXT PRIMARY KEY,
  summary     TEXT NOT NULL,
  actor       TEXT NOT NULL,
  state       TEXT NOT NULL CHECK (state IN ('planned', 'applying', 'applied', 'failed')),
  reverts     TEXT REFERENCES changes (id), -- the change this undoes or rolls back
  fingerprint TEXT NOT NULL,                -- of the plan, so apply runs what was reviewed
  plan        BLOB NOT NULL,                -- the plan as reviewed, for the record
  error       TEXT,
  created_at  INTEGER NOT NULL,
  applied_at  INTEGER
);

-- What a change asks for, resource by resource.
CREATE TABLE change_items (
  change_id     TEXT NOT NULL REFERENCES changes (id),
  position      INTEGER NOT NULL,
  kind          TEXT NOT NULL,
  server_id     TEXT NOT NULL REFERENCES servers (id),
  name          TEXT NOT NULL,
  base_revision INTEGER NOT NULL, -- the revision the edit starts from; 0 for none
  spec          BLOB,             -- NULL deletes
  PRIMARY KEY (change_id, position)
);

-- Every resource Dockwell has managed, deleted ones included, so a name
-- that comes back continues its history.
CREATE TABLE resources (
  id        TEXT PRIMARY KEY,
  kind      TEXT NOT NULL,
  server_id TEXT NOT NULL REFERENCES servers (id),
  name      TEXT NOT NULL,
  revision  INTEGER NOT NULL, -- the latest revision's number
  UNIQUE (kind, server_id, name)
);

-- Each revision of a resource's spec, numbered from 1. Never updated.
CREATE TABLE revisions (
  resource_id TEXT NOT NULL REFERENCES resources (id),
  number      INTEGER NOT NULL,
  spec        BLOB, -- NULL: deleted
  change_id   TEXT NOT NULL REFERENCES changes (id),
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (resource_id, number)
);

CREATE INDEX revisions_change ON revisions (change_id);
