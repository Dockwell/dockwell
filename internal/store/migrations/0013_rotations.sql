-- SPDX-License-Identifier: AGPL-3.0-only
-- Whether each item of a change sets a secret's value. Such an edit is a
-- rotation: its value plans as changed whatever it holds, and is never
-- compared with the one there, so a plan can't confirm a guess at it.
-- Applying plans the change again as it was proposed, so the item keeps
-- it. See docs/adr/0022-secrets-encrypted-at-rest.md.

ALTER TABLE change_items ADD COLUMN rotates INTEGER NOT NULL DEFAULT 0;
