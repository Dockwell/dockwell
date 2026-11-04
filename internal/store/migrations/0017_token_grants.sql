-- SPDX-License-Identifier: AGPL-3.0-only
-- What an API token may do beyond its scope, which its maker has to ask for.
-- A token made before there were any has none, so none of them gains reading
-- logs, which often hold secrets. See docs/adr/0025-logs-per-deploy.md.
ALTER TABLE api_tokens ADD COLUMN grants TEXT NOT NULL DEFAULT '';
