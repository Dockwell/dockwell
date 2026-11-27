-- SPDX-License-Identifier: AGPL-3.0-only

ALTER TABLE builds ADD COLUMN error_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN memory_available_mib BIGINT;
