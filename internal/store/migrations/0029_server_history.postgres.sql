-- SPDX-License-Identifier: AGPL-3.0-only
-- See 0029_server_history.sql.
CREATE TABLE server_history (
  server_id      TEXT COLLATE "C" NOT NULL,
  tier           BIGINT NOT NULL,
  start_at       BIGINT NOT NULL,
  covered_ms     BIGINT NOT NULL,
  cpu_sum        BIGINT NOT NULL, -- thousandths of a percent, by the millisecond
  cpu_max        BIGINT NOT NULL,
  memory_sum     BIGINT NOT NULL, -- KiB in use, by the millisecond
  memory_max     BIGINT NOT NULL,
  memory_total   BIGINT NOT NULL, -- KiB
  swap_sum       BIGINT NOT NULL,
  swap_max       BIGINT NOT NULL,
  load_sum       BIGINT NOT NULL, -- the one-minute load in thousandths, by the millisecond
  load_max       BIGINT NOT NULL,
  disk_sum       BIGINT NOT NULL, -- the fullest disk's share in thousandths, by the millisecond
  disk_max       BIGINT NOT NULL,
  rx_bytes       BIGINT NOT NULL,
  rx_peak        BIGINT NOT NULL, -- the highest bytes a second a sample saw
  tx_bytes       BIGINT NOT NULL,
  tx_peak        BIGINT NOT NULL,
  read_bytes     BIGINT NOT NULL,
  read_peak      BIGINT NOT NULL,
  written_bytes  BIGINT NOT NULL,
  written_peak   BIGINT NOT NULL,
  PRIMARY KEY (server_id, tier, start_at)
);
