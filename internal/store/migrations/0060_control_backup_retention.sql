-- SPDX-License-Identifier: AGPL-3.0-only

ALTER TABLE control_backup_places ADD COLUMN retention_state TEXT NOT NULL DEFAULT '';
