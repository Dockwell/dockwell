-- SPDX-License-Identifier: AGPL-3.0-only
-- The same rules are in 0014_history_is_kept.postgres.sql.

DROP TRIGGER changes_keep_their_plan;
CREATE TRIGGER changes_keep_their_plan
BEFORE UPDATE OF id, summary, actor, reverts, fingerprint, fingerprint_scheme, plan, created_at, proposed_via ON changes
BEGIN SELECT RAISE(ABORT, 'a change keeps the plan that was reviewed: only its state moves on'); END;

CREATE TRIGGER revision_uses_never_change BEFORE UPDATE ON revision_uses
BEGIN SELECT RAISE(ABORT, 'what a revision uses is immutable, as the revision is: write a new revision instead'); END;

CREATE TRIGGER revision_uses_are_kept BEFORE DELETE ON revision_uses
BEGIN SELECT RAISE(ABORT, 'what a revision uses is kept, as the revision is'); END;
