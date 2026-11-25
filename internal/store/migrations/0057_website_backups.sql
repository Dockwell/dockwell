-- SPDX-License-Identifier: AGPL-3.0-only
-- Website folder backups join Backups and scheduled backups alongside volumes.

ALTER TABLE backups ADD COLUMN website_id TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN website_path TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_backups_website ON backups (website_id, started_at DESC, id DESC);
