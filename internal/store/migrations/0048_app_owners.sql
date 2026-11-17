-- SPDX-License-Identifier: AGPL-3.0-only
-- What Dockwell observed of each catalog app's owner it makes, by the app.owner
-- resource's ID: whether it's made, failed, waiting for its app, waiting for an
-- email, or opened by a person before it was. It's an observed fact, never a field
-- a change writes, so a failed step leaves the install held, and only a made or an
-- opened owner lifts the hold. Made and opened are final. The error is the app's
-- answer with every value the steps sent hidden. See
-- docs/adr/0105-a-catalog-apps-owner-is-made-at-install.md.
CREATE TABLE app_owners (
  resource_id TEXT PRIMARY KEY,         -- the app.owner resource's ID
  environment TEXT NOT NULL,            -- its environment's ID
  state       TEXT NOT NULL,            -- waiting_email, waiting, made, failed or opened
  error       TEXT NOT NULL DEFAULT '', -- scrubbed, 512 bytes at most
  tries       INTEGER NOT NULL DEFAULT 0,
  opening     TEXT NOT NULL DEFAULT '', -- the planned change that opens its install, until it applies
  updated_at  BIGINT NOT NULL
);
