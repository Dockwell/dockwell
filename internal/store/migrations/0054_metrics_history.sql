-- SPDX-License-Identifier: AGPL-3.0-only
-- Extends server metrics history with aggregate disk IOPS counters, and adds
-- packed chunked series storage (history_series + history_chunks) for per-disk
-- I/O, per-interface network, and per-container CPU/memory history across
-- 1-minute (24 h), 5-minute (7 d), and 1-hour (31 d) tiers.
--
-- The series and chunk tables live in the dialect files,
-- 0054_metrics_history.sqlite.sql (WITHOUT ROWID) and
-- 0054_metrics_history.postgres.sql (TEXT COLLATE "C", BYTEA), each with an
-- index by (tier, chunk_start) for roll-ups and pruning.

ALTER TABLE server_history ADD COLUMN read_ops INTEGER NOT NULL DEFAULT 0;
ALTER TABLE server_history ADD COLUMN read_iops_peak INTEGER NOT NULL DEFAULT 0;
ALTER TABLE server_history ADD COLUMN write_ops INTEGER NOT NULL DEFAULT 0;
ALTER TABLE server_history ADD COLUMN write_iops_peak INTEGER NOT NULL DEFAULT 0;
