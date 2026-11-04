-- SPDX-License-Identifier: AGPL-3.0-only
-- The same rules are in 0014_history_is_kept.sqlite.sql.

DROP TRIGGER changes_keep_their_plan ON changes;
CREATE TRIGGER changes_keep_their_plan
BEFORE UPDATE OF id, summary, actor, reverts, fingerprint, fingerprint_scheme, plan, created_at, proposed_via ON changes FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('a change keeps the plan that was reviewed: only its state moves on');

CREATE TRIGGER revision_uses_never_change BEFORE UPDATE ON revision_uses FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('what a revision uses is immutable, as the revision is: write a new revision instead');

CREATE TRIGGER revision_uses_are_kept BEFORE DELETE ON revision_uses FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('what a revision uses is kept, as the revision is');
