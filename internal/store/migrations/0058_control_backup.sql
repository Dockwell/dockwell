-- SPDX-License-Identifier: AGPL-3.0-only

CREATE TABLE control_backup (
  singleton       INTEGER PRIMARY KEY CHECK (singleton = 1),
  generation      INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0),
  generation_id   TEXT NOT NULL DEFAULT '',
  position        INTEGER NOT NULL DEFAULT 0,
  changed_at      INTEGER NOT NULL DEFAULT 0,
  first_changed_at INTEGER NOT NULL DEFAULT 0,
  recovery        BLOB,
  recovery_id     TEXT NOT NULL DEFAULT '',
  passphrase_kept INTEGER NOT NULL DEFAULT 0,
  destination     TEXT NOT NULL DEFAULT '',
  destination_pin TEXT NOT NULL DEFAULT '',
  preferred_servers TEXT NOT NULL DEFAULT '',
  excluded_servers TEXT NOT NULL DEFAULT '',
  requested       INTEGER NOT NULL DEFAULT 0,
  last_attempt    INTEGER NOT NULL DEFAULT 0,
  last_position   INTEGER NOT NULL DEFAULT -1,
  hourly          INTEGER NOT NULL DEFAULT 0,
  attempt_problem TEXT NOT NULL DEFAULT ''
);

INSERT INTO control_backup (singleton) VALUES (1);

CREATE TABLE control_backup_places (
  place       TEXT NOT NULL,
  generation  INTEGER NOT NULL,
  target      TEXT NOT NULL,
  sent_at     INTEGER NOT NULL,
  recovery_id TEXT NOT NULL,
  path        TEXT NOT NULL,
  file        TEXT NOT NULL,
  problem     TEXT NOT NULL,
  PRIMARY KEY (generation, place)
);
