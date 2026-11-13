-- SPDX-License-Identifier: AGPL-3.0-only
-- Where what a revision uses lives, when that isn't the resource's own scope: a link's
-- value from another environment, such as the password of a database user Dockwell
-- made on a server in another project. Empty, as every use was before, is the
-- resource's own scope. So finding what uses a value finds a user in any environment,
-- and a change to the value takes it in, in the same plan. See
-- docs/adr/0084-what-an-app-needs-met-by-a-new-service-one-you-run-or-a-connection.md.
ALTER TABLE revision_uses ADD COLUMN used_scope TEXT NOT NULL DEFAULT '';

CREATE INDEX revision_uses_used_elsewhere ON revision_uses (used_scope, used_kind, used_name);

-- A change's item it took in because it uses a value the change changes, as an app
-- restarts with its database user's new password: Dockwell's own, which a role in the
-- app's project isn't needed for. Applying the change, undoing it and every reading
-- of it tell it from the person's edits by this.
ALTER TABLE change_items ADD COLUMN follows INTEGER NOT NULL DEFAULT 0;
