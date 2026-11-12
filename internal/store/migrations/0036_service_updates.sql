-- SPDX-License-Identifier: AGPL-3.0-only
-- What Dockwell last found newer than what each service runs, and what a person
-- set aside of it: the control plane's record, as an alert is, and not managed
-- state, so it has no revisions. A check writes its own columns, and skipping or
-- snoozing writes theirs, so neither undoes the other. See
-- docs/adr/0079-knowing-a-newer-version-of-an-app-is-out.md.
CREATE TABLE service_updates (
  service_id   TEXT PRIMARY KEY,
  server_id    TEXT NOT NULL DEFAULT '', -- the server it was last checked through
  image        TEXT NOT NULL DEFAULT '', -- the reference it was checked for, as its spec named it
  checked_at   BIGINT,                   -- the last check that answered
  tried_at     BIGINT,                   -- the last check, answered or not
  next_at      BIGINT,                   -- when it's checked next
  state        INTEGER NOT NULL DEFAULT 0,
  problem      INTEGER NOT NULL DEFAULT 0,
  detail       TEXT NOT NULL DEFAULT '', -- the registry's or server's words on the problem, a line
  found        TEXT NOT NULL DEFAULT '', -- what's newer, as JSON
  skipped      TEXT NOT NULL DEFAULT '', -- the versions a person skipped, as JSON
  snooze_until BIGINT,
  snoozed_by   TEXT NOT NULL DEFAULT ''
);
