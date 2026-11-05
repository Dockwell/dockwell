-- SPDX-License-Identifier: AGPL-3.0-only
-- Changes nothing. It marks the database as one a control plane that keeps a
-- command's values out of what it reads has used, so a dockwell from before
-- that refuses to open it, rather than read a command with its values written
-- in and carry them into drift and plans. Going back means restoring a backup
-- taken before the upgrade. See docs/adr/0022-secrets-encrypted-at-rest.md.
SELECT 1;
