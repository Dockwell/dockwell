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
