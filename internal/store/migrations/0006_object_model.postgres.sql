-- SPDX-License-Identifier: AGPL-3.0-only
-- History is never rewritten (ADR 0009): revisions and what a change asked
-- for are written once, a resource only moves on to its next revision, and
-- a deployment keeps its revision and server. The same rules are in
-- 0006_object_model.sqlite.sql.

-- Refuses the statement with the trigger's first argument as the message.
CREATE FUNCTION dockwell_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION USING MESSAGE = TG_ARGV[0], ERRCODE = 'restrict_violation';
END
$$;

CREATE TRIGGER revisions_never_change BEFORE UPDATE ON revisions FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('revisions are immutable: write a new revision instead');

CREATE TRIGGER revisions_are_kept BEFORE DELETE ON revisions FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('revisions are kept: a deletion is a new revision');

CREATE TRIGGER change_items_never_change BEFORE UPDATE ON change_items FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('the items of a change are immutable: plan a new change instead');

CREATE TRIGGER change_items_are_kept BEFORE DELETE ON change_items FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('the items of a change are kept');

CREATE TRIGGER changes_keep_their_plan BEFORE UPDATE OF id, summary, actor, reverts, fingerprint, plan, created_at ON changes FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('a change keeps the plan that was reviewed: only its state moves on');

CREATE TRIGGER changes_are_kept BEFORE DELETE ON changes FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('changes are kept');

CREATE TRIGGER resources_move_forward BEFORE UPDATE ON resources FOR EACH ROW
WHEN (NEW.id IS DISTINCT FROM OLD.id OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.scope IS DISTINCT FROM OLD.scope
  OR NEW.name IS DISTINCT FROM OLD.name OR NEW.revision IS DISTINCT FROM OLD.revision + 1)
EXECUTE FUNCTION dockwell_refuse('a resource only moves on to its next revision');

CREATE TRIGGER resources_are_kept BEFORE DELETE ON resources FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('resources are kept: a deletion is a new revision');

CREATE TRIGGER deployments_keep_their_place BEFORE UPDATE OF resource_id, revision, server_id, created_at ON deployments FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('a deployment keeps its revision and server: only its state moves on');

CREATE TRIGGER deployments_are_kept BEFORE DELETE ON deployments FOR EACH ROW
EXECUTE FUNCTION dockwell_refuse('deployments are kept');
