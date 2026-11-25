-- SPDX-License-Identifier: AGPL-3.0-only

CREATE TABLE file_access_undo (
  event_id TEXT PRIMARY KEY REFERENCES audit_events (id) ON DELETE CASCADE,
  data BLOB NOT NULL -- dockwell.v1.ChangeFileAccessResponse, with only its Undo entries
);
