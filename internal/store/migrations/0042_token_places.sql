-- SPDX-License-Identifier: AGPL-3.0-only
-- What a token is narrowed to (ADR 0071): the projects it acts in, their IDs
-- apart by commas, NULL for every project its maker holds, which every token
-- made before this acts in; a list, even an empty one, is exactly those, so one
-- emptied by a cleanup or a hand edit reaches no project rather than every one.
-- And the role it acts as at most, empty for its maker's. See
-- docs/adr/0071-api-tokens-narrowed-to-projects-and-a-role.md.
ALTER TABLE api_tokens ADD COLUMN projects TEXT;
ALTER TABLE api_tokens ADD COLUMN role TEXT NOT NULL DEFAULT '' CHECK (role IN ('', 'developer', 'viewer'));
