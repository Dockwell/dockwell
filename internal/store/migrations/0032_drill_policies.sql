-- SPDX-License-Identifier: AGPL-3.0-only
-- The backup policy a restore drill ran for, as its schedule (ADR 0068). Empty for
-- one someone started.
ALTER TABLE backup_drills ADD COLUMN policy_id TEXT NOT NULL DEFAULT '';

CREATE INDEX backup_drills_policy ON backup_drills (policy_id, server_id, started_at DESC);
