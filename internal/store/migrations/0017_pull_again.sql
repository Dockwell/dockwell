-- SPDX-License-Identifier: AGPL-3.0-only

-- A deploy of the tag a service already names pulls it again (ADR 0026), and a
-- change's item says so, as a change is applied from its record. What a server
-- ran is kept with each deployment, by digest, so a rollback of a moving tag can
-- put back what it really was.
ALTER TABLE change_items ADD COLUMN pull INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deployments ADD COLUMN digest TEXT NOT NULL DEFAULT '';
