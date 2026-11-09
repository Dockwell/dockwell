-- SPDX-License-Identifier: AGPL-3.0-only
-- Who may do what, and where: a role in a place, the install or one project
-- (docs/adr/0048-members-as-far-as-one-owner-goes.md,
-- docs/adr/0049-roles-and-invites-by-project.md). An empty project_id is the
-- install, which covers every project. The owner and admins hold the install
-- only. A person holds one role in each place, and in a project the highest of
-- theirs that covers it. There is one owner, as on users.
CREATE TABLE grants (
  user_id    TEXT NOT NULL REFERENCES users (id),
  project_id TEXT NOT NULL DEFAULT '',
  role       TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'developer', 'viewer')),
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL DEFAULT '', -- the actor who granted it, as the audit log names actors
  PRIMARY KEY (user_id, project_id),
  CHECK (role IN ('developer', 'viewer') OR project_id = '')
);
CREATE UNIQUE INDEX grants_one_owner ON grants (role) WHERE role = 'owner';
CREATE INDEX grants_by_project ON grants (project_id);

-- The owner who claimed the install holds it already.
INSERT INTO grants (user_id, project_id, role, created_at, created_by)
  SELECT id, '', 'owner', created_at, 'dockwell' FROM users WHERE role = 'owner';

-- A person's role is their grants', so it leaves the users table.
DROP INDEX users_one_owner;
ALTER TABLE users DROP COLUMN role;
