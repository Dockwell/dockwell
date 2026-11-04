-- SPDX-License-Identifier: AGPL-3.0-only
-- A pending authenticator secret belongs to the setup that made it: the
-- session whose passkey confirmed it, for 10 minutes, and 5 wrong codes.
-- Until now it waited on the user's row for any session to guess at, for
-- ever, and ended with new recovery codes. See
-- docs/adr/0022-secrets-encrypted-at-rest.md.

ALTER TABLE users ADD COLUMN totp_pending_session TEXT;
ALTER TABLE users ADD COLUMN totp_pending_expires_at INTEGER;
ALTER TABLE users ADD COLUMN totp_pending_failures INTEGER NOT NULL DEFAULT 0;

-- One left from before has no session, so nothing could finish it.
UPDATE users SET totp_pending = NULL;
