-- SPDX-License-Identifier: AGPL-3.0-only

ALTER TABLE control_backup ADD COLUMN destination_selection TEXT NOT NULL DEFAULT '';
ALTER TABLE control_backup ADD COLUMN destination_marker TEXT NOT NULL DEFAULT '';
ALTER TABLE control_backup_places ADD COLUMN name TEXT NOT NULL DEFAULT '';
ALTER TABLE control_backup_places ADD COLUMN previous_pending INTEGER NOT NULL DEFAULT 0;
ALTER TABLE control_backup_places ADD COLUMN dismissed INTEGER NOT NULL DEFAULT 0;

UPDATE control_backup_places SET place = 'offsite:' || target,
  name = COALESCE((SELECT destination FROM control_backup WHERE destination_pin = control_backup_places.target), '')
WHERE place = 'offsite';
