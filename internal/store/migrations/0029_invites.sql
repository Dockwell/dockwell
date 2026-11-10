-- SPDX-License-Identifier: AGPL-3.0-only
-- Invites to sign in with a role in a place, the install or one project
-- (docs/adr/0048-members-as-far-as-one-owner-goes.md,
-- docs/adr/0049-roles-and-invites-by-project.md). An invite is access, not
-- managed state, so it's audited, not a revision. Its secret is kept as a hash,
-- and it works once. Nobody is invited as owner, and an admin's place is the
-- install, as grants have it.
CREATE TABLE invites (
  id          TEXT PRIMARY KEY,
  token_hash  BLOB NOT NULL UNIQUE,
  email       TEXT NOT NULL,
  role        TEXT NOT NULL CHECK (role IN ('admin', 'developer', 'viewer')),
  project_id  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL, -- the actor who made it, as the audit log names actors
  expires_at  INTEGER NOT NULL,
  accepted_at INTEGER,
  accepted_by TEXT REFERENCES users (id),
  revoked_at  INTEGER,
  revoked_by  TEXT NOT NULL DEFAULT '',
  CHECK (role IN ('developer', 'viewer') OR project_id = '')
);
CREATE INDEX invites_open ON invites (expires_at) WHERE accepted_at IS NULL AND revoked_at IS NULL;
