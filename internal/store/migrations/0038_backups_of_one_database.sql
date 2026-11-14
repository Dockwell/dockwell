-- SPDX-License-Identifier: AGPL-3.0-only
-- The one database a backup holds, for an app's database on a server it shares,
-- which the app's page backs up and restores alone. Empty, as every backup was
-- before, holds the whole volume. See
-- docs/adr/0084-what-an-app-needs-met-by-a-new-service-one-you-run-or-a-connection.md.
-- for_link is the link, by ID, of the app it was taken for, whose project's role
-- lists and restores it.
ALTER TABLE backups ADD COLUMN one_database TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN for_link TEXT NOT NULL DEFAULT '';
