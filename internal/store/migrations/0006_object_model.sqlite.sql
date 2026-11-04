-- SPDX-License-Identifier: AGPL-3.0-only
-- History is never rewritten (ADR 0009): revisions and what a change asked
-- for are written once, a resource only moves on to its next revision, and
-- a deployment keeps its revision and server. The same rules are in
-- 0006_object_model.postgres.sql.

CREATE TRIGGER revisions_never_change BEFORE UPDATE ON revisions
BEGIN SELECT RAISE(ABORT, 'revisions are immutable: write a new revision instead'); END;

CREATE TRIGGER revisions_are_kept BEFORE DELETE ON revisions
BEGIN SELECT RAISE(ABORT, 'revisions are kept: a deletion is a new revision'); END;

CREATE TRIGGER change_items_never_change BEFORE UPDATE ON change_items
BEGIN SELECT RAISE(ABORT, 'the items of a change are immutable: plan a new change instead'); END;

CREATE TRIGGER change_items_are_kept BEFORE DELETE ON change_items
BEGIN SELECT RAISE(ABORT, 'the items of a change are kept'); END;

CREATE TRIGGER changes_keep_their_plan BEFORE UPDATE OF id, summary, actor, reverts, fingerprint, plan, created_at ON changes
BEGIN SELECT RAISE(ABORT, 'a change keeps the plan that was reviewed: only its state moves on'); END;

CREATE TRIGGER changes_are_kept BEFORE DELETE ON changes
BEGIN SELECT RAISE(ABORT, 'changes are kept'); END;

CREATE TRIGGER resources_move_forward BEFORE UPDATE ON resources
WHEN NEW.id IS NOT OLD.id OR NEW.kind IS NOT OLD.kind OR NEW.scope IS NOT OLD.scope OR NEW.name IS NOT OLD.name
  OR NEW.revision IS NOT OLD.revision + 1
BEGIN SELECT RAISE(ABORT, 'a resource only moves on to its next revision'); END;

CREATE TRIGGER resources_are_kept BEFORE DELETE ON resources
BEGIN SELECT RAISE(ABORT, 'resources are kept: a deletion is a new revision'); END;

CREATE TRIGGER deployments_keep_their_place BEFORE UPDATE OF resource_id, revision, server_id, created_at ON deployments
BEGIN SELECT RAISE(ABORT, 'a deployment keeps its revision and server: only its state moves on'); END;

CREATE TRIGGER deployments_are_kept BEFORE DELETE ON deployments
BEGIN SELECT RAISE(ABORT, 'deployments are kept'); END;
