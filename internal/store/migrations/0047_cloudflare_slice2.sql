-- SPDX-License-Identifier: AGPL-3.0-only
-- What Dockwell keeps at Cloudflare beyond records and rules: the mode it set a
-- zone to, and what a zone's mode was before; and the server a tunnel's rule was
-- written for, so the drift timer can tell when the tunnel runs somewhere else too.
-- See docs/adr/0104-adding-a-domain-publishes-it.md.

-- A record's backup kept apart (type 'pending') can share its name with another
-- one, so only records written at Cloudflare are one per name and type.
DROP INDEX cloudflare_records_name;
CREATE UNIQUE INDEX cloudflare_records_name ON cloudflare_records (zone_id, name, type) WHERE type <> 'pending';

-- The server whose proxy a rule sends to, by ID: empty for a rule written before
-- this was kept.
ALTER TABLE cloudflare_rules ADD COLUMN server_id TEXT NOT NULL DEFAULT '';

-- A zone Dockwell set to Full (strict) for an admin, with the mode it had before,
-- which taking it off the provider's strict zones puts back.
CREATE TABLE cloudflare_zone_modes (
  zone_id    TEXT PRIMARY KEY,
  provider   TEXT NOT NULL,       -- the DNS provider whose token set it, by name
  zone       TEXT NOT NULL,       -- such as "example.com"
  was        TEXT NOT NULL,       -- the mode before, such as "flexible"
  change_id  TEXT NOT NULL,
  set_at     INTEGER NOT NULL
);
