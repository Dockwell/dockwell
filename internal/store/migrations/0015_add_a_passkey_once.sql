-- SPDX-License-Identifier: AGPL-3.0-only
-- A sign-in that needed no passkey here, such as with a recovery code or a
-- link from dockwell recover, confirms adding one passkey in its first
-- minutes, as a step-up would, and only one. This records when its session
-- used it, so a second can't, across restarts and racing requests alike.
-- See docs/adr/0022-secrets-encrypted-at-rest.md.

ALTER TABLE sessions ADD COLUMN added_passkey_at INTEGER;
