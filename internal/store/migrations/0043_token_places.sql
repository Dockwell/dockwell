-- SPDX-License-Identifier: AGPL-3.0-only
-- What a token is narrowed to (ADR 0071): the projects it acts in, their IDs
-- apart by commas, NULL for every project its maker holds, which every token
-- made before this acts in; a list, even an empty one, is exactly those, so one
-- emptied by a cleanup or a hand edit reaches no project rather than every one.
-- And the role it acts as at most, empty for its maker's. See
-- docs/adr/0071-api-tokens-narrowed-to-projects-and-a-role.md.
ALTER TABLE api_tokens ADD COLUMN projects TEXT;
ALTER TABLE api_tokens ADD COLUMN role TEXT NOT NULL DEFAULT '' CHECK (role IN ('', 'developer', 'viewer'));

-- A deploy-on-push hook is narrowed as a token is, to its service's project, as a
-- developer at most. A hook made before this takes the project its service is in
-- now, or none if its service is gone.
ALTER TABLE git_hooks ADD COLUMN projects TEXT;
ALTER TABLE git_hooks ADD COLUMN role TEXT NOT NULL DEFAULT 'developer' CHECK (role IN ('', 'developer', 'viewer'));
UPDATE git_hooks SET projects = COALESCE((
  SELECT environment.scope FROM resources service
  JOIN resources environment ON environment.id = service.scope
  WHERE service.id = git_hooks.resource_id AND environment.kind = 'environment'
), '');
