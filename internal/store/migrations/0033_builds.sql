-- SPDX-License-Identifier: AGPL-3.0-only
-- What the control plane knows of each build of a service's image, as its server
-- says, and the lines its log keeps: the first 1,000 and the last 9,000, as a
-- build's container is gone once it ends, and its log is how anyone finds out why it
-- failed. A build is a step of a change, not managed state, so it has no revisions.
-- See docs/adr/0046-builds-from-git.md.
CREATE TABLE builds (
  id             TEXT PRIMARY KEY,
  change_id      TEXT NOT NULL,
  resource_id    TEXT NOT NULL,            -- the service's ID
  environment_id TEXT NOT NULL,
  service        TEXT NOT NULL,            -- its name, which outlives it
  server_id      TEXT NOT NULL,
  revision       BIGINT NOT NULL DEFAULT 0,
  state          TEXT NOT NULL CHECK (state IN ('queued', 'building', 'done', 'failed', 'cancelled', 'lost')),
  url            TEXT NOT NULL DEFAULT '',
  ref            TEXT NOT NULL DEFAULT '',
  commit_id      TEXT NOT NULL DEFAULT '',
  commit_subject TEXT NOT NULL DEFAULT '',
  commit_author  TEXT NOT NULL DEFAULT '',
  image          TEXT NOT NULL DEFAULT '',
  steps          BIGINT NOT NULL DEFAULT 0,
  cached_steps   BIGINT NOT NULL DEFAULT 0,
  step           BIGINT NOT NULL DEFAULT 0,
  step_name      TEXT NOT NULL DEFAULT '',
  error          TEXT NOT NULL DEFAULT '',
  failed_line    TEXT NOT NULL DEFAULT '',
  cancelled_by   TEXT NOT NULL DEFAULT '',
  log_lines      BIGINT NOT NULL DEFAULT 0,
  log_left_out   BIGINT NOT NULL DEFAULT 0,
  log_dropped    BIGINT NOT NULL DEFAULT 0,
  created_at     BIGINT NOT NULL,
  started_at     BIGINT,
  ended_at       BIGINT
);

CREATE UNIQUE INDEX builds_step ON builds (change_id, resource_id, server_id);
CREATE INDEX builds_service ON builds (resource_id, created_at DESC, id DESC);

CREATE TABLE build_lines (
  build_id TEXT NOT NULL,
  number   BIGINT NOT NULL,
  step     BIGINT NOT NULL DEFAULT 0,
  at       BIGINT NOT NULL,
  line     TEXT NOT NULL,
  PRIMARY KEY (build_id, number)
);
