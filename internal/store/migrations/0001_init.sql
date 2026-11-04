-- SPDX-License-Identifier: AGPL-3.0-only
-- The control plane's first schema: its identity, the servers it manages,
-- the tickets that let new servers join, and an audit trail.
-- Times are Unix milliseconds (UTC). The SQL stays portable to Postgres.

-- Singletons, such as the install's ID and the sealed CA.
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      BLOB NOT NULL,
  updated_at INTEGER NOT NULL
);

-- Servers that run the agent. A revoked server keeps its row for history.
CREATE TABLE servers (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  cert_serial    TEXT NOT NULL,
  cert_not_after INTEGER NOT NULL,
  last_seen_at   INTEGER,
  revoked_at     INTEGER
);

CREATE UNIQUE INDEX servers_active_name ON servers (name) WHERE revoked_at IS NULL;

-- Single-use tickets that let a server join. Only a hash of the secret is kept.
CREATE TABLE join_tokens (
  id          TEXT PRIMARY KEY,
  secret_hash BLOB NOT NULL,
  created_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL,
  expires_at  INTEGER NOT NULL,
  used_at     INTEGER,
  used_by     TEXT REFERENCES servers (id)
);

-- Who did what, from where. Append-only.
CREATE TABLE audit_events (
  id      TEXT PRIMARY KEY,
  at      INTEGER NOT NULL,
  actor   TEXT NOT NULL,
  action  TEXT NOT NULL,
  subject TEXT NOT NULL,
  detail  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX audit_events_at ON audit_events (at);
