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
-- What only making the owner needed goes once it's made or opened, as observed
-- state the control plane works towards each look: the secrets the owner held
-- while it was made, and variables its entry takes out, as Vaultwarden's
-- ADMIN_TOKEN. settled says it's done, so a later write that sets one again on
-- purpose stays. settle_tries, settle_next and settle_since are how often it
-- failed, when it's tried again, and since when; unset_left the variables still
-- set, and settle_alerted that Activity has said so, once.
ALTER TABLE app_owners ADD COLUMN settled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE app_owners ADD COLUMN settle_tries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE app_owners ADD COLUMN settle_next BIGINT NOT NULL DEFAULT 0;
ALTER TABLE app_owners ADD COLUMN settle_since BIGINT NOT NULL DEFAULT 0;
ALTER TABLE app_owners ADD COLUMN settle_error TEXT NOT NULL DEFAULT '';
ALTER TABLE app_owners ADD COLUMN unset_left TEXT NOT NULL DEFAULT '';
ALTER TABLE app_owners ADD COLUMN settle_alerted INTEGER NOT NULL DEFAULT 0;
