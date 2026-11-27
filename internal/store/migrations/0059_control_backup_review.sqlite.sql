-- SPDX-License-Identifier: AGPL-3.0-only

CREATE TRIGGER control_backup_git_hooks_insert AFTER INSERT ON git_hooks
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_git_hooks_update AFTER UPDATE ON git_hooks
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_git_hooks_delete AFTER DELETE ON git_hooks
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_github_installations_insert AFTER INSERT ON github_installations
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_github_installations_update AFTER UPDATE ON github_installations
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_github_installations_delete AFTER DELETE ON github_installations
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_provider_links_insert AFTER INSERT ON provider_links
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_provider_links_update AFTER UPDATE ON provider_links
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_provider_links_delete AFTER DELETE ON provider_links
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_tunnels_insert AFTER INSERT ON cloudflare_tunnels
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_tunnels_update AFTER UPDATE ON cloudflare_tunnels
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_tunnels_delete AFTER DELETE ON cloudflare_tunnels
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_records_insert AFTER INSERT ON cloudflare_records
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_records_update AFTER UPDATE ON cloudflare_records
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_records_delete AFTER DELETE ON cloudflare_records
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_rules_insert AFTER INSERT ON cloudflare_rules
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_rules_update AFTER UPDATE ON cloudflare_rules
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_rules_delete AFTER DELETE ON cloudflare_rules
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_zone_modes_insert AFTER INSERT ON cloudflare_zone_modes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_zone_modes_update AFTER UPDATE ON cloudflare_zone_modes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_cloudflare_zone_modes_delete AFTER DELETE ON cloudflare_zone_modes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_key_encryption_keys_insert AFTER INSERT ON key_encryption_keys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_key_encryption_keys_update AFTER UPDATE ON key_encryption_keys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_key_encryption_keys_delete AFTER DELETE ON key_encryption_keys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_data_keys_insert AFTER INSERT ON data_keys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_data_keys_update AFTER UPDATE ON data_keys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_data_keys_delete AFTER DELETE ON data_keys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_backups_insert AFTER INSERT ON backups
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_backups_update AFTER UPDATE ON backups
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_backups_delete AFTER DELETE ON backups
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_backup_drills_insert AFTER INSERT ON backup_drills
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_backup_drills_update AFTER UPDATE ON backup_drills
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_backup_drills_delete AFTER DELETE ON backup_drills
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_app_owners_insert AFTER INSERT ON app_owners
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_app_owners_update AFTER UPDATE ON app_owners
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_app_owners_delete AFTER DELETE ON app_owners
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_alert_mutes_insert AFTER INSERT ON alert_mutes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_alert_mutes_update AFTER UPDATE ON alert_mutes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_alert_mutes_delete AFTER DELETE ON alert_mutes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_code_guards_insert AFTER INSERT ON code_guards
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_code_guards_update AFTER UPDATE ON code_guards
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_code_guards_delete AFTER DELETE ON code_guards
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_pinned_folders_insert AFTER INSERT ON pinned_folders
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_pinned_folders_update AFTER UPDATE ON pinned_folders
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_pinned_folders_delete AFTER DELETE ON pinned_folders
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_service_updates_insert AFTER INSERT ON service_updates
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_service_updates_update AFTER UPDATE ON service_updates
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_service_updates_delete AFTER DELETE ON service_updates
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_change_servers_insert AFTER INSERT ON change_servers
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_change_servers_update AFTER UPDATE ON change_servers
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_change_servers_delete AFTER DELETE ON change_servers
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_revision_uses_insert AFTER INSERT ON revision_uses
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_revision_uses_update AFTER UPDATE ON revision_uses
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_revision_uses_delete AFTER DELETE ON revision_uses
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
