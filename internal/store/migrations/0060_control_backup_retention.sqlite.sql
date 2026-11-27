-- SPDX-License-Identifier: AGPL-3.0-only

-- SQLite requires one trigger for each event. Share the position update so
-- readers parse only a short trigger body for each event.
CREATE VIEW control_backup_tick AS SELECT singleton FROM control_backup;
CREATE TRIGGER control_backup_tick_insert INSTEAD OF INSERT ON control_backup_tick
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;

DROP TRIGGER control_backup_resources_insert;
CREATE TRIGGER control_backup_resources_insert AFTER INSERT ON resources
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_resources_update;
CREATE TRIGGER control_backup_resources_update AFTER UPDATE ON resources
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_resources_delete;
CREATE TRIGGER control_backup_resources_delete AFTER DELETE ON resources
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_revisions_insert;
CREATE TRIGGER control_backup_revisions_insert AFTER INSERT ON revisions
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_revisions_update;
CREATE TRIGGER control_backup_revisions_update AFTER UPDATE ON revisions
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_revisions_delete;
CREATE TRIGGER control_backup_revisions_delete AFTER DELETE ON revisions
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_change_items_insert;
CREATE TRIGGER control_backup_change_items_insert AFTER INSERT ON change_items
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_change_items_update;
CREATE TRIGGER control_backup_change_items_update AFTER UPDATE ON change_items
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_change_items_delete;
CREATE TRIGGER control_backup_change_items_delete AFTER DELETE ON change_items
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_changes_insert;
CREATE TRIGGER control_backup_changes_insert AFTER INSERT ON changes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_changes_update;
CREATE TRIGGER control_backup_changes_update AFTER UPDATE ON changes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_changes_delete;
CREATE TRIGGER control_backup_changes_delete AFTER DELETE ON changes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_deployments_insert;
CREATE TRIGGER control_backup_deployments_insert AFTER INSERT ON deployments
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_deployments_update;
CREATE TRIGGER control_backup_deployments_update AFTER UPDATE ON deployments
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_deployments_delete;
CREATE TRIGGER control_backup_deployments_delete AFTER DELETE ON deployments
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_organizations_insert;
CREATE TRIGGER control_backup_organizations_insert AFTER INSERT ON organizations
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_organizations_update;
CREATE TRIGGER control_backup_organizations_update AFTER UPDATE ON organizations
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_organizations_delete;
CREATE TRIGGER control_backup_organizations_delete AFTER DELETE ON organizations
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_users_insert;
CREATE TRIGGER control_backup_users_insert AFTER INSERT ON users
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_users_update;
CREATE TRIGGER control_backup_users_update AFTER UPDATE ON users
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_users_delete;
CREATE TRIGGER control_backup_users_delete AFTER DELETE ON users
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_grants_insert;
CREATE TRIGGER control_backup_grants_insert AFTER INSERT ON grants
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_grants_update;
CREATE TRIGGER control_backup_grants_update AFTER UPDATE ON grants
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_grants_delete;
CREATE TRIGGER control_backup_grants_delete AFTER DELETE ON grants
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_passkeys_insert;
CREATE TRIGGER control_backup_passkeys_insert AFTER INSERT ON passkeys
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_passkeys_update;
CREATE TRIGGER control_backup_passkeys_update AFTER UPDATE ON passkeys
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_passkeys_delete;
CREATE TRIGGER control_backup_passkeys_delete AFTER DELETE ON passkeys
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_invites_insert;
CREATE TRIGGER control_backup_invites_insert AFTER INSERT ON invites
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_invites_update;
CREATE TRIGGER control_backup_invites_update AFTER UPDATE ON invites
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_invites_delete;
CREATE TRIGGER control_backup_invites_delete AFTER DELETE ON invites
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_recovery_codes_insert;
CREATE TRIGGER control_backup_recovery_codes_insert AFTER INSERT ON recovery_codes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_recovery_codes_update;
CREATE TRIGGER control_backup_recovery_codes_update AFTER UPDATE ON recovery_codes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_recovery_codes_delete;
CREATE TRIGGER control_backup_recovery_codes_delete AFTER DELETE ON recovery_codes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_settings_insert;
CREATE TRIGGER control_backup_settings_insert AFTER INSERT ON settings
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_settings_update;
CREATE TRIGGER control_backup_settings_update AFTER UPDATE ON settings
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_settings_delete;
CREATE TRIGGER control_backup_settings_delete AFTER DELETE ON settings
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_api_tokens_insert;
CREATE TRIGGER control_backup_api_tokens_insert AFTER INSERT ON api_tokens
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_api_tokens_update;
CREATE TRIGGER control_backup_api_tokens_update AFTER UPDATE OF revoked_at ON api_tokens
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_api_tokens_delete;
CREATE TRIGGER control_backup_api_tokens_delete AFTER DELETE ON api_tokens
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_servers_insert;
CREATE TRIGGER control_backup_servers_insert AFTER INSERT ON servers
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_servers_update;
CREATE TRIGGER control_backup_servers_update AFTER UPDATE OF name, cert_serial, cert_not_after, revoked_at ON servers
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_servers_delete;
CREATE TRIGGER control_backup_servers_delete AFTER DELETE ON servers
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_sessions_update;
CREATE TRIGGER control_backup_sessions_update AFTER UPDATE OF revoked_at ON sessions
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_join_tokens_insert;
CREATE TRIGGER control_backup_join_tokens_insert AFTER INSERT ON join_tokens
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_join_tokens_update;
CREATE TRIGGER control_backup_join_tokens_update AFTER UPDATE OF used_at ON join_tokens
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_join_tokens_delete;
CREATE TRIGGER control_backup_join_tokens_delete AFTER DELETE ON join_tokens
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_link_tokens_insert;
CREATE TRIGGER control_backup_link_tokens_insert AFTER INSERT ON link_tokens
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_link_tokens_update;
CREATE TRIGGER control_backup_link_tokens_update AFTER UPDATE OF used_at, revoked_at ON link_tokens
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_link_tokens_delete;
CREATE TRIGGER control_backup_link_tokens_delete AFTER DELETE ON link_tokens
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_git_hooks_insert;
CREATE TRIGGER control_backup_git_hooks_insert AFTER INSERT ON git_hooks
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_git_hooks_update;
CREATE TRIGGER control_backup_git_hooks_update AFTER UPDATE ON git_hooks
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_git_hooks_delete;
CREATE TRIGGER control_backup_git_hooks_delete AFTER DELETE ON git_hooks
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_github_installations_insert;
CREATE TRIGGER control_backup_github_installations_insert AFTER INSERT ON github_installations
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_github_installations_update;
CREATE TRIGGER control_backup_github_installations_update AFTER UPDATE ON github_installations
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_github_installations_delete;
CREATE TRIGGER control_backup_github_installations_delete AFTER DELETE ON github_installations
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_provider_links_insert;
CREATE TRIGGER control_backup_provider_links_insert AFTER INSERT ON provider_links
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_provider_links_update;
CREATE TRIGGER control_backup_provider_links_update AFTER UPDATE ON provider_links
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_provider_links_delete;
CREATE TRIGGER control_backup_provider_links_delete AFTER DELETE ON provider_links
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_tunnels_insert;
CREATE TRIGGER control_backup_cloudflare_tunnels_insert AFTER INSERT ON cloudflare_tunnels
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_tunnels_update;
CREATE TRIGGER control_backup_cloudflare_tunnels_update AFTER UPDATE ON cloudflare_tunnels
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_tunnels_delete;
CREATE TRIGGER control_backup_cloudflare_tunnels_delete AFTER DELETE ON cloudflare_tunnels
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_records_insert;
CREATE TRIGGER control_backup_cloudflare_records_insert AFTER INSERT ON cloudflare_records
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_records_update;
CREATE TRIGGER control_backup_cloudflare_records_update AFTER UPDATE ON cloudflare_records
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_records_delete;
CREATE TRIGGER control_backup_cloudflare_records_delete AFTER DELETE ON cloudflare_records
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_rules_insert;
CREATE TRIGGER control_backup_cloudflare_rules_insert AFTER INSERT ON cloudflare_rules
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_rules_update;
CREATE TRIGGER control_backup_cloudflare_rules_update AFTER UPDATE ON cloudflare_rules
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_rules_delete;
CREATE TRIGGER control_backup_cloudflare_rules_delete AFTER DELETE ON cloudflare_rules
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_zone_modes_insert;
CREATE TRIGGER control_backup_cloudflare_zone_modes_insert AFTER INSERT ON cloudflare_zone_modes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_zone_modes_update;
CREATE TRIGGER control_backup_cloudflare_zone_modes_update AFTER UPDATE ON cloudflare_zone_modes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_cloudflare_zone_modes_delete;
CREATE TRIGGER control_backup_cloudflare_zone_modes_delete AFTER DELETE ON cloudflare_zone_modes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_key_encryption_keys_insert;
CREATE TRIGGER control_backup_key_encryption_keys_insert AFTER INSERT ON key_encryption_keys
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_key_encryption_keys_update;
CREATE TRIGGER control_backup_key_encryption_keys_update AFTER UPDATE ON key_encryption_keys
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_key_encryption_keys_delete;
CREATE TRIGGER control_backup_key_encryption_keys_delete AFTER DELETE ON key_encryption_keys
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_data_keys_insert;
CREATE TRIGGER control_backup_data_keys_insert AFTER INSERT ON data_keys
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_data_keys_update;
CREATE TRIGGER control_backup_data_keys_update AFTER UPDATE ON data_keys
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_data_keys_delete;
CREATE TRIGGER control_backup_data_keys_delete AFTER DELETE ON data_keys
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_backups_insert;
CREATE TRIGGER control_backup_backups_insert AFTER INSERT ON backups
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_backups_update;
CREATE TRIGGER control_backup_backups_update AFTER UPDATE ON backups
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_backups_delete;
CREATE TRIGGER control_backup_backups_delete AFTER DELETE ON backups
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_backup_drills_insert;
CREATE TRIGGER control_backup_backup_drills_insert AFTER INSERT ON backup_drills
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_backup_drills_update;
CREATE TRIGGER control_backup_backup_drills_update AFTER UPDATE ON backup_drills
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_backup_drills_delete;
CREATE TRIGGER control_backup_backup_drills_delete AFTER DELETE ON backup_drills
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_app_owners_insert;
CREATE TRIGGER control_backup_app_owners_insert AFTER INSERT ON app_owners
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_app_owners_update;
CREATE TRIGGER control_backup_app_owners_update AFTER UPDATE ON app_owners
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_app_owners_delete;
CREATE TRIGGER control_backup_app_owners_delete AFTER DELETE ON app_owners
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_alert_mutes_insert;
CREATE TRIGGER control_backup_alert_mutes_insert AFTER INSERT ON alert_mutes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_alert_mutes_update;
CREATE TRIGGER control_backup_alert_mutes_update AFTER UPDATE ON alert_mutes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_alert_mutes_delete;
CREATE TRIGGER control_backup_alert_mutes_delete AFTER DELETE ON alert_mutes
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_code_guards_insert;
CREATE TRIGGER control_backup_code_guards_insert AFTER INSERT ON code_guards
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_code_guards_update;
CREATE TRIGGER control_backup_code_guards_update AFTER UPDATE ON code_guards
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_code_guards_delete;
CREATE TRIGGER control_backup_code_guards_delete AFTER DELETE ON code_guards
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_pinned_folders_insert;
CREATE TRIGGER control_backup_pinned_folders_insert AFTER INSERT ON pinned_folders
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_pinned_folders_update;
CREATE TRIGGER control_backup_pinned_folders_update AFTER UPDATE ON pinned_folders
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_pinned_folders_delete;
CREATE TRIGGER control_backup_pinned_folders_delete AFTER DELETE ON pinned_folders
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_service_updates_insert;
CREATE TRIGGER control_backup_service_updates_insert AFTER INSERT ON service_updates
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_service_updates_update;
CREATE TRIGGER control_backup_service_updates_update AFTER UPDATE ON service_updates
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_service_updates_delete;
CREATE TRIGGER control_backup_service_updates_delete AFTER DELETE ON service_updates
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_change_servers_insert;
CREATE TRIGGER control_backup_change_servers_insert AFTER INSERT ON change_servers
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_change_servers_update;
CREATE TRIGGER control_backup_change_servers_update AFTER UPDATE ON change_servers
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_change_servers_delete;
CREATE TRIGGER control_backup_change_servers_delete AFTER DELETE ON change_servers
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_revision_uses_insert;
CREATE TRIGGER control_backup_revision_uses_insert AFTER INSERT ON revision_uses
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_revision_uses_update;
CREATE TRIGGER control_backup_revision_uses_update AFTER UPDATE ON revision_uses
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
DROP TRIGGER control_backup_revision_uses_delete;
CREATE TRIGGER control_backup_revision_uses_delete AFTER DELETE ON revision_uses
  BEGIN INSERT INTO control_backup_tick VALUES (1); END;
