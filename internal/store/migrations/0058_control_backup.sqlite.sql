-- SPDX-License-Identifier: AGPL-3.0-only

-- Position moves in the same transaction as a managed write.
CREATE TRIGGER control_backup_resources_insert AFTER INSERT ON resources
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_resources_update AFTER UPDATE ON resources
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_resources_delete AFTER DELETE ON resources
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_revisions_insert AFTER INSERT ON revisions
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_revisions_update AFTER UPDATE ON revisions
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_revisions_delete AFTER DELETE ON revisions
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_change_items_insert AFTER INSERT ON change_items
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_change_items_update AFTER UPDATE ON change_items
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_change_items_delete AFTER DELETE ON change_items
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_changes_insert AFTER INSERT ON changes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_changes_update AFTER UPDATE ON changes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_changes_delete AFTER DELETE ON changes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_deployments_insert AFTER INSERT ON deployments
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_deployments_update AFTER UPDATE ON deployments
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_deployments_delete AFTER DELETE ON deployments
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_organizations_insert AFTER INSERT ON organizations
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_organizations_update AFTER UPDATE ON organizations
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_organizations_delete AFTER DELETE ON organizations
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_users_insert AFTER INSERT ON users
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_users_update AFTER UPDATE ON users
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_users_delete AFTER DELETE ON users
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_grants_insert AFTER INSERT ON grants
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_grants_update AFTER UPDATE ON grants
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_grants_delete AFTER DELETE ON grants
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_passkeys_insert AFTER INSERT ON passkeys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_passkeys_update AFTER UPDATE ON passkeys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_passkeys_delete AFTER DELETE ON passkeys
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_invites_insert AFTER INSERT ON invites
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_invites_update AFTER UPDATE ON invites
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_invites_delete AFTER DELETE ON invites
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_recovery_codes_insert AFTER INSERT ON recovery_codes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_recovery_codes_update AFTER UPDATE ON recovery_codes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_recovery_codes_delete AFTER DELETE ON recovery_codes
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_settings_insert AFTER INSERT ON settings
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_settings_update AFTER UPDATE ON settings
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_settings_delete AFTER DELETE ON settings
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_api_tokens_insert AFTER INSERT ON api_tokens
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_api_tokens_update AFTER UPDATE OF revoked_at ON api_tokens
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_api_tokens_delete AFTER DELETE ON api_tokens
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_servers_insert AFTER INSERT ON servers
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_servers_update AFTER UPDATE OF name, cert_serial, cert_not_after, revoked_at ON servers
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_servers_delete AFTER DELETE ON servers
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_sessions_update AFTER UPDATE OF revoked_at ON sessions
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_join_tokens_insert AFTER INSERT ON join_tokens
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_join_tokens_update AFTER UPDATE OF used_at ON join_tokens
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_join_tokens_delete AFTER DELETE ON join_tokens
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_link_tokens_insert AFTER INSERT ON link_tokens
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_link_tokens_update AFTER UPDATE OF used_at, revoked_at ON link_tokens
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
CREATE TRIGGER control_backup_link_tokens_delete AFTER DELETE ON link_tokens
  BEGIN UPDATE control_backup SET position = position + 1,
    changed_at = CAST(unixepoch('subsec') * 1000 AS INTEGER),
    first_changed_at = CASE WHEN first_changed_at = 0 THEN CAST(unixepoch('subsec') * 1000 AS INTEGER) ELSE first_changed_at END WHERE singleton = 1; END;
