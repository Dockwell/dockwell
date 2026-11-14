-- SPDX-License-Identifier: AGPL-3.0-only
-- Accounts at sign-in providers that sign their person in to the panel, kept by
-- the provider's issuer and the account's subject, never by an email address,
-- and the link a session began from, if it did. See
-- docs/adr/0095-signing-in-to-the-panel-with-a-provider.md.
CREATE TABLE provider_links (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users (id),
  provider_id  TEXT NOT NULL,
  issuer       TEXT NOT NULL,
  subject      TEXT NOT NULL,
  account      TEXT NOT NULL,   -- what the provider calls the account, to tell them apart
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER
);

-- One account at an issuer signs in one person, and a person links one account
-- at each provider.
CREATE UNIQUE INDEX provider_links_account ON provider_links (issuer, subject);
CREATE UNIQUE INDEX provider_links_person ON provider_links (user_id, provider_id);

ALTER TABLE sessions ADD COLUMN provider_link_id TEXT NOT NULL DEFAULT '';
