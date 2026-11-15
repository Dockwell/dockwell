-- SPDX-License-Identifier: AGPL-3.0-only
-- Where a GitHub App Dockwell made is installed, as GitHub says (ADR 0098): the
-- account, which repositories it may see, and whether it's suspended or removed.
-- Facts GitHub reports, as its installation events and the App's own reading say
-- them, not desired state: which projects an installation reaches is the App's
-- spec. And a service's hook that GitHub's App delivers pushes to, which has no
-- secret of its own to show. See docs/adr/0098-connecting-github-as-an-app.md.
CREATE TABLE github_installations (
  id           BIGINT PRIMARY KEY,           -- GitHub's ID for it
  app_id       TEXT NOT NULL,                -- the github.app resource's ID
  account      TEXT NOT NULL,
  organization BOOLEAN NOT NULL DEFAULT FALSE,
  selection    TEXT NOT NULL DEFAULT '',     -- "all" or "selected"
  repositories TEXT NOT NULL DEFAULT '[]',   -- JSON, as GitHub last listed them
  suspended_at BIGINT,
  removed_at   BIGINT,
  updated_at   BIGINT NOT NULL
);

CREATE INDEX github_installations_app ON github_installations (app_id);

-- How a hook's pushes come: '' through its own URL and secret, "github" through
-- the GitHub App that reaches its repository.
ALTER TABLE git_hooks ADD COLUMN via TEXT NOT NULL DEFAULT '';
