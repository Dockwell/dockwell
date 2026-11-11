-- SPDX-License-Identifier: AGPL-3.0-only
-- Restore drills: each check that a backup restores, in a container and a volume it
-- throws away. A drill is an event, as a backup is, not managed state, so this is
-- written outside the revision store, and a drill's rows go with its backup, but for
-- a failed one, which stays until a backup at least as new passes, so the alert it
-- raised holds. See docs/adr/0068-restore-drills.md.
CREATE TABLE backup_drills (
  id             TEXT PRIMARY KEY,
  backup_id      TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  volume_id      TEXT NOT NULL,
  server_id      TEXT NOT NULL,
  result         TEXT NOT NULL CHECK (result IN ('running', 'passed', 'failed', 'not_checked')),
  why            TEXT NOT NULL DEFAULT '',
  detail         TEXT NOT NULL DEFAULT '',
  restored_bytes BIGINT NOT NULL DEFAULT 0,
  tables_checked BIGINT NOT NULL DEFAULT 0,
  unpacked_only  BOOLEAN NOT NULL DEFAULT FALSE,
  actor          TEXT NOT NULL,
  started_at     BIGINT NOT NULL,
  finished_at    BIGINT,
  -- backup_at is when its backup was taken, which outlasts the backup.
  backup_at      BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX backup_drills_backup ON backup_drills (backup_id, started_at DESC);
CREATE INDEX backup_drills_volume ON backup_drills (volume_id, started_at DESC);
CREATE INDEX backup_drills_result ON backup_drills (result);
