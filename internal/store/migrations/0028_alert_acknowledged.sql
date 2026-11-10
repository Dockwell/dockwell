-- SPDX-License-Identifier: AGPL-3.0-only
-- Who said they're on an alert, and when: an audited action, as a mute is, and
-- not managed state. Unset while nobody has; a resolved alert keeps what it had.
-- See docs/adr/0044-alerts.md.
ALTER TABLE alerts ADD COLUMN acknowledged_at BIGINT;
ALTER TABLE alerts ADD COLUMN acknowledged_by TEXT NOT NULL DEFAULT '';
