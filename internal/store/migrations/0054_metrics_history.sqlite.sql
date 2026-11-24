-- SPDX-License-Identifier: AGPL-3.0-only
-- See 0054_metrics_history.sql.
-- history_series maps each (server_id, kind, name) series (kind: 1=disk,
-- 2=net, 3=container) to a compact integer ID so chunks don't repeat 36-byte
-- server IDs and 64-byte container IDs on every bucket.
CREATE TABLE history_series (
  id         INTEGER PRIMARY KEY,
  server_id  TEXT NOT NULL,
  kind       INTEGER NOT NULL,
  name       TEXT NOT NULL,
  UNIQUE (server_id, kind, name)
);

-- history_chunks packs multiple time buckets of one series and tier into one
-- varint-encoded binary chunk (1-hour chunks for 1-minute points, 1-day chunks
-- for 5-minute and 1-hour points), eliminating per-point B-tree row overhead.
CREATE TABLE history_chunks (
  series_id    INTEGER NOT NULL,
  tier         INTEGER NOT NULL,
  chunk_start  BIGINT NOT NULL,
  data         BLOB NOT NULL,
  PRIMARY KEY (series_id, tier, chunk_start)
) WITHOUT ROWID;
CREATE INDEX history_chunks_by_time ON history_chunks (tier, chunk_start);
