-- SPDX-License-Identifier: AGPL-3.0-only

ALTER TABLE api_tokens ADD COLUMN deploy_service TEXT NOT NULL DEFAULT '';
ALTER TABLE api_tokens ADD COLUMN deploy_environment TEXT NOT NULL DEFAULT '';

CREATE TABLE ci_deploy_keys (
  service_id TEXT NOT NULL,
  digest TEXT NOT NULL,
  commit_sha TEXT NOT NULL,
  change_id TEXT NOT NULL UNIQUE REFERENCES changes(id) ON DELETE CASCADE,
  PRIMARY KEY (service_id, digest, commit_sha)
);

-- The key moves between attempts. Each change keeps its own immutable origin.
CREATE TABLE ci_deploy_origins (
  change_id TEXT PRIMARY KEY REFERENCES changes(id) ON DELETE CASCADE,
  service_id TEXT NOT NULL,
  provider INTEGER NOT NULL,
  repository TEXT NOT NULL,
  commit_sha TEXT NOT NULL,
  ref TEXT NOT NULL,
  run_url TEXT NOT NULL,
  commit_url TEXT NOT NULL,
  image TEXT NOT NULL,
  digest TEXT NOT NULL
);
