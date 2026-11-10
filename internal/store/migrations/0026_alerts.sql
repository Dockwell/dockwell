-- SPDX-License-Identifier: AGPL-3.0-only
-- What the control plane's alert rules held for, and what it sent of it. An alert
-- is a rule holding for one thing, such as a server's disk, from when it fired
-- until it resolved: the control plane's record, as a backup's row is, and not
-- managed state, so it has no revisions. Rules and channels are resources. See
-- docs/adr/0044-alerts.md.
CREATE TABLE alerts (
  id            TEXT PRIMARY KEY,
  rule_id       TEXT NOT NULL,
  rule_name     TEXT NOT NULL,            -- which outlives the rule
  thing         TEXT NOT NULL,            -- what it's about, one alert of a rule at a time
  condition     INTEGER NOT NULL,
  severity      INTEGER NOT NULL,
  server_id     TEXT NOT NULL DEFAULT '',
  subject_id    TEXT NOT NULL DEFAULT '',
  subject_name  TEXT NOT NULL DEFAULT '',
  title         TEXT NOT NULL,
  detail        TEXT NOT NULL,
  path          TEXT NOT NULL DEFAULT '',
  facts         TEXT NOT NULL DEFAULT '', -- what the title says, as JSON, for the panel to say in its language
  opened_at     BIGINT NOT NULL,
  resolved_at   BIGINT,
  ended         TEXT NOT NULL DEFAULT '', -- why it closed, when it didn't resolve: its rule was removed or turned off
  sent_at       BIGINT,                   -- when its firing message was last queued for its channels
  resolved_sent BOOLEAN NOT NULL DEFAULT FALSE -- once its resolved message was queued, or never will be
);

CREATE UNIQUE INDEX alerts_one_open ON alerts (rule_id, thing) WHERE resolved_at IS NULL;
CREATE INDEX alerts_opened ON alerts (opened_at DESC, id DESC);
CREATE INDEX alerts_rule ON alerts (rule_id, opened_at DESC);

-- Each message to a channel, and what the far end answered. Nothing that signs in.
CREATE TABLE alert_deliveries (
  id           TEXT PRIMARY KEY,
  channel_id   TEXT NOT NULL,
  channel_name TEXT NOT NULL,
  at           BIGINT NOT NULL,
  ok           BOOLEAN NOT NULL,
  said         TEXT NOT NULL,
  about        TEXT NOT NULL
);

CREATE INDEX alert_deliveries_channel ON alert_deliveries (channel_id, at DESC);
CREATE INDEX alert_deliveries_at ON alert_deliveries (at DESC, id DESC);

-- The outbox: each message an alert owes a channel, firing or resolved, until the
-- far end takes it or Dockwell gives up. A rule's sends that are due at once go as
-- one message. A restart resumes what's left, so a message may arrive twice, never
-- not at all.
CREATE TABLE alert_sends (
  id         TEXT PRIMARY KEY,
  alert_id   TEXT NOT NULL,
  rule_id    TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  status     TEXT NOT NULL,              -- firing or resolved
  queued_at  BIGINT NOT NULL,
  due_at     BIGINT NOT NULL,            -- when it's next tried
  tries      INTEGER NOT NULL DEFAULT 0,
  done_at    BIGINT,                     -- when the far end took it, or Dockwell gave up
  ok         BOOLEAN NOT NULL DEFAULT FALSE,
  said       TEXT NOT NULL DEFAULT ''    -- what the far end last answered, in a line
);

CREATE UNIQUE INDEX alert_sends_one ON alert_sends (alert_id, channel_id, status) WHERE done_at IS NULL;
CREATE INDEX alert_sends_due ON alert_sends (due_at) WHERE done_at IS NULL;
CREATE INDEX alert_sends_alert ON alert_sends (alert_id);

-- What a mute holds: a rule, by name, as "rule:<name>", or a server, as
-- "server:<id>", until a time.
CREATE TABLE alert_mutes (
  target     TEXT PRIMARY KEY,
  until_at   BIGINT NOT NULL,
  actor      TEXT NOT NULL,
  reason     TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL
);
