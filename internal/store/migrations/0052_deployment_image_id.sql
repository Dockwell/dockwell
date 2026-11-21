-- SPDX-License-Identifier: AGPL-3.0-only

ALTER TABLE deployments ADD COLUMN image_id TEXT NOT NULL DEFAULT '';
