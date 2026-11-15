-- SPDX-License-Identifier: AGPL-3.0-only
-- A service's hook, which deploys it on a push from GitHub, Gitea or Forgejo: a
-- credential, as an API token is, acting for the person who made it, with its
-- secret sealed. And what each delivery came to, by the provider's ID for it, so a
-- delivery seen before does nothing. See docs/adr/0046-builds-from-git.md.
CREATE TABLE git_hooks (
  id          TEXT PRIMARY KEY,
  resource_id TEXT NOT NULL UNIQUE, -- the service's ID
  user_id     TEXT NOT NULL,        -- who made it, which it acts for
  secret      BLOB NOT NULL,        -- sealed
  created_at  BIGINT NOT NULL,
  secret_at   BIGINT NOT NULL
);

CREATE TABLE git_hook_deliveries (
  hook_id     TEXT NOT NULL,
  delivery_id TEXT NOT NULL,
  at          BIGINT NOT NULL,
  provider    TEXT NOT NULL,
  ref         TEXT NOT NULL DEFAULT '',
  commit_id   TEXT NOT NULL DEFAULT '',
  outcome     TEXT NOT NULL,
  change_id   TEXT NOT NULL DEFAULT '',
  detail      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (hook_id, delivery_id)
);

CREATE INDEX git_hook_deliveries_at ON git_hook_deliveries (hook_id, at DESC);
