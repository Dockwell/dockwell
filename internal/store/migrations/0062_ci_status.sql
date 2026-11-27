-- SPDX-License-Identifier: AGPL-3.0-only

ALTER TABLE github_installations ADD COLUMN permissions TEXT NOT NULL DEFAULT '{}';

-- Durable outbox for reporting CI deploy transitions back to GitHub and GitLab.
-- Provider installation and credential references live here, apart from pipeline
-- provenance in ci_deploy_origins.
CREATE TABLE ci_status_outbox (
  id TEXT PRIMARY KEY,
  change_id TEXT NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
  transition INTEGER NOT NULL,
  state TEXT NOT NULL,
  service_id TEXT NOT NULL,
  service_name TEXT NOT NULL DEFAULT '',
  provider INTEGER NOT NULL,
  repository TEXT NOT NULL,
  commit_sha TEXT NOT NULL,
  ref TEXT NOT NULL,
  run_url TEXT NOT NULL,
  digest TEXT NOT NULL,
  change_url TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT NOT NULL UNIQUE,
  delivery_state TEXT NOT NULL,
  remote_id TEXT NOT NULL DEFAULT '',
  remote_url TEXT NOT NULL DEFAULT '',
  credential_ref TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  retry_since INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  delivered_at INTEGER,
  UNIQUE (change_id, transition)
);

CREATE INDEX ci_status_outbox_due ON ci_status_outbox (delivery_state, next_attempt_at, change_id, transition);
CREATE INDEX ci_status_outbox_change ON ci_status_outbox (change_id, transition DESC);
