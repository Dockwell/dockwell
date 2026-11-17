-- SPDX-License-Identifier: AGPL-3.0-only
-- What Dockwell wrote at Cloudflare for routes, or took on there, so it removes only
-- what it wrote, while it still reads as written, and undo writes back what it
-- replaced. See docs/adr/0104-adding-a-domain-publishes-it.md.

-- A DNS record, by its ID at Cloudflare, as Dockwell wrote it: what it found there
-- later is compared with this.
CREATE TABLE cloudflare_records (
  id          TEXT PRIMARY KEY,   -- the record's ID at Cloudflare
  provider    TEXT NOT NULL,      -- the DNS provider whose token wrote it, by name
  zone_id     TEXT NOT NULL,
  zone        TEXT NOT NULL,      -- such as "example.com"
  name        TEXT NOT NULL,      -- such as "wiki.example.com"
  type        TEXT NOT NULL,      -- CNAME, A or AAAA
  content     TEXT NOT NULL,
  proxied     INTEGER NOT NULL,
  ttl         INTEGER NOT NULL,
  comment     TEXT NOT NULL,      -- "dockwell <install-id> <route-id>"
  route_id    TEXT NOT NULL,      -- the route it publishes, by resource ID
  change_id   TEXT NOT NULL,      -- the change that wrote it, or took it on
  taken_on_at INTEGER,            -- set for a record made by hand and taken on
  replaced    TEXT NOT NULL DEFAULT '', -- the record it replaced, whole, as JSON, for undo
  state       TEXT NOT NULL DEFAULT 'published', -- published, gone, changed, tunnel_deleted or unreadable, as last read
  checked_at  INTEGER,
  created_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX cloudflare_records_name ON cloudflare_records (zone_id, name, type);
CREATE INDEX cloudflare_records_route ON cloudflare_records (route_id);

-- A tunnel's rule, which has no ID at Cloudflare, by its tunnel, hostname and path:
-- a zone's wildcard or its apex, which every route in the zone through that tunnel
-- shares, or a rule made by hand and taken on.
CREATE TABLE cloudflare_rules (
  tunnel_id   TEXT NOT NULL,
  hostname    TEXT NOT NULL,
  path        TEXT NOT NULL,
  account     TEXT NOT NULL,
  provider    TEXT NOT NULL,
  zone        TEXT NOT NULL,
  service     TEXT NOT NULL,      -- where it sends, as written
  read_doc    TEXT NOT NULL,      -- the tunnel's document as read before the write
  written_doc TEXT NOT NULL,      -- and as written
  version     INTEGER NOT NULL,   -- the document's version Cloudflare gave the write
  change_id   TEXT NOT NULL,
  taken_on_at INTEGER,
  replaced    TEXT NOT NULL DEFAULT '', -- the rule as it was before Move it to Dockwell, as JSON
  state       TEXT NOT NULL DEFAULT 'published',
  checked_at  INTEGER,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (tunnel_id, hostname, path)
);

-- Each tunnel Dockwell has written to: the version of its document last read, so
-- the drift timer acts only when it moves, and the lock that makes Dockwell one
-- writer of it, with a lease that runs out, so two control planes during an upgrade
-- can't both hold it and a stopped one doesn't hold it for ever.
CREATE TABLE cloudflare_tunnels (
  tunnel_id    TEXT PRIMARY KEY,
  account      TEXT NOT NULL DEFAULT '',
  seen_version INTEGER NOT NULL DEFAULT -1,
  checked_at   INTEGER,
  lock_holder  TEXT NOT NULL DEFAULT '',
  lock_until   INTEGER NOT NULL DEFAULT 0
);
