-- SPDX-License-Identifier: AGPL-3.0-only
-- Where a backup went beside its server's disk: the destination's name, which
-- outlives it as a volume's does, what it's called there, and what became of it. The
-- bytes there are sealed before they leave, and nothing here says what signs in.
-- off_server says only the destination has it now. See
-- docs/adr/0033-backups-of-volumes-and-databases.md.
ALTER TABLE backups ADD COLUMN destination TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN copy_key TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN copy_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN copy_error TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN off_server BOOLEAN NOT NULL DEFAULT FALSE;
