-- SPDX-License-Identifier: AGPL-3.0-only
-- The labels a server joining with a token gets, as JSON of key to value, which
-- Dockwell sets on its host as it makes it. Empty is none. See
-- docs/adr/0091-server-labels.md.
ALTER TABLE join_tokens ADD COLUMN labels TEXT NOT NULL DEFAULT '';
