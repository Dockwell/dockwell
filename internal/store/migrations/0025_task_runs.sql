-- SPDX-License-Identifier: AGPL-3.0-only
-- What each scheduled task's runs did, as their servers' agents said. Each agent
-- keeps its tasks' time and runs them, and keeps their runs until the control
-- plane reads them; this keeps each task's newest 100 on each server, with what
-- they printed, so a list answers with a server offline. A run is an event, not
-- managed state, so it has no revisions. See docs/adr/0053-scheduled-tasks.md.
CREATE TABLE task_runs (
  id         TEXT PRIMARY KEY,
  task_id    TEXT NOT NULL,   -- the task's resource ID
  server_id  TEXT NOT NULL,
  state      INTEGER NOT NULL, -- dockwell.v1.TaskRunState
  started_at BIGINT NOT NULL,
  ended_at   BIGINT,          -- NULL while it runs
  run        BLOB NOT NULL    -- the dockwell.v1.TaskRun, with its attempts and output
);

CREATE INDEX task_runs_by_task ON task_runs (task_id, server_id, started_at DESC, id DESC);
