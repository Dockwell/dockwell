-- SPDX-License-Identifier: AGPL-3.0-only

CREATE TABLE pinned_folders (
  user_id   TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  server_id TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
  path      TEXT NOT NULL,
  path_hash TEXT NOT NULL,
  PRIMARY KEY (user_id, server_id, path_hash)
);
