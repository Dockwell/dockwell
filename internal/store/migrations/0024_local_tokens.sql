-- SPDX-License-Identifier: AGPL-3.0-only
-- Who asked for a token the CLI made for local access on the control server,
-- as "mira@hz-1", so the audit log and the timeline say it came from there
-- without reading the token's name, which says whatever its maker typed.
-- Empty for every other token, and for a local one made before this, which
-- lasted 12 hours and is gone. See docs/adr/0047-the-audit-log-for-a-person.md.
ALTER TABLE api_tokens ADD COLUMN local_account TEXT NOT NULL DEFAULT '';
