-- SPDX-License-Identifier: AGPL-3.0-only
-- What the change API and the timeline need of a change. See
-- docs/adr/0017-the-change-api-and-the-agent-stream.md.
--
-- A change can now be discarded, which needs a new state; the dialect
-- files widen the check on changes.state. A change also records where it
-- was proposed from, who applied it and from where, and the progress of
-- its steps. Audit events record where they came from.

ALTER TABLE changes ADD COLUMN proposed_via TEXT NOT NULL DEFAULT ''; -- panel, api or automatic
ALTER TABLE changes ADD COLUMN applied_by TEXT;                      -- the actor who applied it
ALTER TABLE changes ADD COLUMN applied_via TEXT;
ALTER TABLE changes ADD COLUMN progress BLOB;                        -- each step and how it went, JSON

ALTER TABLE audit_events ADD COLUMN surface TEXT NOT NULL DEFAULT '';
