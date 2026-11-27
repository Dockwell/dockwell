-- SPDX-License-Identifier: AGPL-3.0-only

CREATE TRIGGER control_backup_ci_deploy_keys_insert AFTER INSERT ON ci_deploy_keys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_ci_deploy_keys_update AFTER UPDATE ON ci_deploy_keys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_ci_deploy_keys_delete AFTER DELETE ON ci_deploy_keys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_ci_deploy_origins_insert AFTER INSERT ON ci_deploy_origins
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_ci_deploy_origins_update AFTER UPDATE ON ci_deploy_origins
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_ci_deploy_origins_delete AFTER DELETE ON ci_deploy_origins
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
