-- SPDX-License-Identifier: AGPL-3.0-only
-- Whether each item of a change sets values that may be secret, such as a
-- secret's value or a container's environment. Such an edit is a
-- rotation: its values plan as changed whatever they hold, and are never
-- compared with the ones there, so a plan can't confirm a guess at one.
-- Applying plans the change again as it was proposed, so the item keeps
-- it. See docs/adr/0022-secrets-encrypted-at-rest.md.

ALTER TABLE change_items ADD COLUMN rotates INTEGER NOT NULL DEFAULT 0;
