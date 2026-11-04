-- SPDX-License-Identifier: AGPL-3.0-only
-- Postgres changes the check on changes.state in place. The same rules are
-- in 0008_changes.sqlite.sql.

ALTER TABLE changes DROP CONSTRAINT changes_state_check;
ALTER TABLE changes ADD CONSTRAINT changes_state_check
  CHECK (state IN ('planned', 'applying', 'applied', 'failed', 'discarded'));

CREATE INDEX changes_newest ON changes (created_at, id);
CREATE INDEX changes_reverts ON changes (reverts);

DROP TRIGGER changes_keep_their_plan ON changes;
CREATE TRIGGER changes_keep_their_plan BEFORE UPDATE OF id, summary, actor, reverts, fingerprint, plan, created_at, proposed_via ON changes FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('a change keeps the plan that was reviewed: only its state moves on');
