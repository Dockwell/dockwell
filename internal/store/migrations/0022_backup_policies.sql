-- SPDX-License-Identifier: AGPL-3.0-only
-- Which policy took each backup, so its retention reaches only its own: a backup
-- someone took by hand is theirs. See docs/adr/0033-backups-of-volumes-and-databases.md.
ALTER TABLE backups ADD COLUMN policy_id TEXT NOT NULL DEFAULT '';

CREATE INDEX backups_policy ON backups (policy_id, started_at DESC);
