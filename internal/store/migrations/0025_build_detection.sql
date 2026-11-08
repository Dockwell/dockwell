-- SPDX-License-Identifier: AGPL-3.0-only
-- What a build found as it read its commit: what built it, its Dockerfile or
-- Railpack, and with Railpack, what it installed and ran, as JSON. Empty until its
-- server says. See docs/adr/0046-builds-from-git.md.
ALTER TABLE builds ADD COLUMN detection TEXT NOT NULL DEFAULT '';
