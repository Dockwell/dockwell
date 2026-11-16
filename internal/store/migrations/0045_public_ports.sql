-- SPDX-License-Identifier: AGPL-3.0-only
-- The ports a change's item makes public on purpose, as its catalog entry declares,
-- such as a proxy's 80 and 443, with spaces between ("80 443/udp"): each is host
-- access a person approves, which applying the change plans again. Empty, as every
-- item was before, makes none. See
-- docs/adr/0101-a-port-a-catalog-app-publishes-to-everyone-on-purpose.md.
ALTER TABLE change_items ADD COLUMN public_ports TEXT NOT NULL DEFAULT '';
