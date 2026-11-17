-- SPDX-License-Identifier: AGPL-3.0-only
-- A new password for a catalog app's owner while its entry's reset steps set it:
-- sealed, beside the install's current one, until it's known which the app takes,
-- by the steps' answer or, where that's lost, by the owner's done signing in with
-- one of them. The one that signs in stays; the other goes. rotation names this
-- try, which the sealed value is bound to. rotate_error is what the app said where
-- it couldn't be set, scrubbed. See
-- docs/adr/0105-a-catalog-apps-owner-is-made-at-install.md.
ALTER TABLE app_owners ADD COLUMN pending BLOB;
ALTER TABLE app_owners ADD COLUMN rotation TEXT NOT NULL DEFAULT '';
ALTER TABLE app_owners ADD COLUMN rotate_error TEXT NOT NULL DEFAULT '';
ALTER TABLE app_owners ADD COLUMN rotate_tries INTEGER NOT NULL DEFAULT 0;
