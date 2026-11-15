-- SPDX-License-Identifier: AGPL-3.0-only
-- Managing members (docs/adr/0049-roles-and-invites-by-project.md, "Managing
-- members"). Someone removed keeps their row, so Activity and the audit log
-- still say who did what, and their email is free again: it's unique among
-- those who sign in. An invite keeps the address it was accepted from, for
-- Members to say where someone joined from.
ALTER TABLE users ADD COLUMN removed_at INTEGER;
ALTER TABLE users ADD COLUMN removed_by TEXT NOT NULL DEFAULT '';
DROP INDEX users_email;
CREATE UNIQUE INDEX users_email ON users (email) WHERE removed_at IS NULL;
ALTER TABLE invites ADD COLUMN accepted_from TEXT NOT NULL DEFAULT '';
