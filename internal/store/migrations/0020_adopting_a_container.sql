-- SPDX-License-Identifier: AGPL-3.0-only

-- A change can take on a container Dockwell didn't make, and a change is applied
-- from its record (ADR 0031).
ALTER TABLE change_items ADD COLUMN adopts TEXT NOT NULL DEFAULT '';
ALTER TABLE change_items ADD COLUMN adopts_name TEXT NOT NULL DEFAULT '';
