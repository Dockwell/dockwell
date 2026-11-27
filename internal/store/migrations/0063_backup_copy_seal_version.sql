-- SPDX-License-Identifier: AGPL-3.0-only

ALTER TABLE backups ADD COLUMN copy_seal_version INTEGER NOT NULL DEFAULT 0;
