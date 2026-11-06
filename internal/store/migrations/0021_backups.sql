-- SPDX-License-Identifier: AGPL-3.0-only
-- What the control plane knows of each backup. The bytes stay on the server that
-- took them, and this answers lists with that server offline. A backup is an
-- event, not managed state, so it has no revisions. A failed backup left nothing
-- on its server, and stays here until someone deletes it, as what it says is why
-- it failed. See docs/adr/0033-backups-of-volumes-and-databases.md.
CREATE TABLE backups (
  id             TEXT PRIMARY KEY,
  environment_id TEXT NOT NULL,
  volume         TEXT NOT NULL,            -- its name, which outlives it
  volume_id      TEXT NOT NULL,
  server_id      TEXT NOT NULL,
  state          TEXT NOT NULL CHECK (state IN ('running', 'done', 'failed')),
  method         TEXT NOT NULL DEFAULT '',
  database_name  TEXT NOT NULL DEFAULT '',
  size_bytes     BIGINT NOT NULL DEFAULT 0,
  sha256         TEXT NOT NULL DEFAULT '',
  files_changed  BIGINT NOT NULL DEFAULT 0,
  actor          TEXT NOT NULL,
  error          TEXT NOT NULL DEFAULT '',
  started_at     BIGINT NOT NULL,
  finished_at    BIGINT
);

CREATE INDEX backups_started ON backups (started_at DESC, id DESC);
CREATE INDEX backups_volume ON backups (volume_id, started_at DESC);
CREATE INDEX backups_environment ON backups (environment_id, started_at DESC);
