-- SPDX-License-Identifier: AGPL-3.0-only
-- A record made by hand that Dockwell keeps a copy of (type 'pending') can share its
-- name with another such copy, as a dual-stack name's A and AAAA both replaced
-- leave them. Only records written at Cloudflare are one per name and type. See
-- docs/adr/0104-adding-a-domain-publishes-it.md.
DROP INDEX cloudflare_records_name;
CREATE UNIQUE INDEX cloudflare_records_name ON cloudflare_records (zone_id, name, type) WHERE type <> 'pending';
