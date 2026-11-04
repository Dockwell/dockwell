-- SPDX-License-Identifier: AGPL-3.0-only
-- API tokens for scripts, the CLI and integrations. See
-- docs/adr/0016-scoped-api-tokens.md and docs/security/sign-in.md.

-- Each token acts for the user who made it, within its scope. Only a hash of
-- the secret is kept.
CREATE TABLE api_tokens (
  id           TEXT PRIMARY KEY,
  token_hash   BLOB NOT NULL UNIQUE,
  user_id      TEXT NOT NULL REFERENCES users (id),
  name         TEXT NOT NULL,
  hint         TEXT NOT NULL, -- the prefix and the first and last 4 random digits, such as dwt_4f9K…x9Ta
  scope        TEXT NOT NULL CHECK (scope IN ('read_only', 'read_write')),
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,                -- unset: it doesn't expire
  last_used_at INTEGER,
  last_ip      TEXT NOT NULL DEFAULT '',
  revoked_at   INTEGER
);

CREATE INDEX api_tokens_user ON api_tokens (user_id);

-- The addresses each token has been used from, an IPv6 /64 counting as one,
-- so the first use from a new one is audited. At most 256 per token, the
-- least recently seen going first.
CREATE TABLE api_token_addresses (
  token_id   TEXT NOT NULL REFERENCES api_tokens (id) ON DELETE CASCADE,
  address    TEXT NOT NULL,
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  PRIMARY KEY (token_id, address)
);
