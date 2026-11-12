-- SPDX-License-Identifier: AGPL-3.0-only
-- How often each person is asked for their passkey again, and when a passkey
-- last confirmed each session: signing in with one, or a step-up. A session
-- from before has none, so it's asked as before until it confirms. See
-- docs/adr/0075-how-often-a-passkey-is-asked-for-again.md.
ALTER TABLE users ADD COLUMN step_up_policy TEXT NOT NULL DEFAULT 'each_time';
ALTER TABLE sessions ADD COLUMN passkey_at INTEGER;
