-- SPDX-License-Identifier: AGPL-3.0-only
-- SQLite can't change a check constraint in place, so changes is rebuilt
-- with discarded among its states, as 0006 rebuilt the M1 tables: its rows
-- are copied aside, and the table made again under its own name, so the
-- rows that refer to it find it again. Foreign keys are checked at commit,
-- by when every change is back. Dropping the table drops its triggers,
-- which are made again as 0006 had them.

PRAGMA defer_foreign_keys = ON;

CREATE TABLE changes_before AS SELECT * FROM changes;
DROP TABLE changes;

CREATE TABLE changes (
  id           TEXT PRIMARY KEY,
  summary      TEXT NOT NULL,
  actor        TEXT NOT NULL,
  state        TEXT NOT NULL CHECK (state IN ('planned', 'applying', 'applied', 'failed', 'discarded')),
  reverts      TEXT REFERENCES changes (id), -- the change this undoes or rolls back
  fingerprint  TEXT NOT NULL,                -- of the plan, so apply runs what was reviewed
  plan         BLOB NOT NULL,                -- the plan as reviewed, for the record
  error        TEXT,
  created_at   INTEGER NOT NULL,
  applied_at   INTEGER,
  proposed_via TEXT NOT NULL DEFAULT '',
  applied_by   TEXT,
  applied_via  TEXT,
  progress     BLOB
);

INSERT INTO changes (id, summary, actor, state, reverts, fingerprint, plan, error, created_at, applied_at, proposed_via, applied_by, applied_via, progress)
  SELECT id, summary, actor, state, reverts, fingerprint, plan, error, created_at, applied_at, proposed_via, applied_by, applied_via, progress
  FROM changes_before ORDER BY created_at, id;
DROP TABLE changes_before;

CREATE INDEX changes_newest ON changes (created_at, id);
CREATE INDEX changes_reverts ON changes (reverts);
-- Resume lists the changes left applying on every start, connect and wakeup.
CREATE INDEX changes_by_state ON changes (state, created_at, id);

CREATE TRIGGER changes_keep_their_plan BEFORE UPDATE OF id, summary, actor, reverts, fingerprint, plan, created_at, proposed_via ON changes
BEGIN SELECT RAISE(ABORT, 'a change keeps the plan that was reviewed: only its state moves on'); END;

CREATE TRIGGER changes_are_kept BEFORE DELETE ON changes
BEGIN SELECT RAISE(ABORT, 'changes are kept'); END;
