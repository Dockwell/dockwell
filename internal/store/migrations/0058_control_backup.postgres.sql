-- SPDX-License-Identifier: AGPL-3.0-only

-- Position moves in the same transaction as a managed write.
CREATE FUNCTION control_backup_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE control_backup SET position = position + 1,
    changed_at = floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint,
    first_changed_at = CASE WHEN first_changed_at = 0 THEN floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint ELSE first_changed_at END WHERE singleton = 1;
  RETURN NULL;
END;
$$;

CREATE TRIGGER control_backup_resources AFTER INSERT OR UPDATE OR DELETE ON resources
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_revisions AFTER INSERT OR UPDATE OR DELETE ON revisions
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_change_items AFTER INSERT OR UPDATE OR DELETE ON change_items
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_changes AFTER INSERT OR UPDATE OR DELETE ON changes
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_deployments AFTER INSERT OR UPDATE OR DELETE ON deployments
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_organizations AFTER INSERT OR UPDATE OR DELETE ON organizations
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_users AFTER INSERT OR UPDATE OR DELETE ON users
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_grants AFTER INSERT OR UPDATE OR DELETE ON grants
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_passkeys AFTER INSERT OR UPDATE OR DELETE ON passkeys
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_invites AFTER INSERT OR UPDATE OR DELETE ON invites
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_recovery_codes AFTER INSERT OR UPDATE OR DELETE ON recovery_codes
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_settings AFTER INSERT OR UPDATE OR DELETE ON settings
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_api_tokens AFTER INSERT OR UPDATE OF revoked_at OR DELETE ON api_tokens
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_servers AFTER INSERT OR UPDATE OF name, cert_serial, cert_not_after, revoked_at OR DELETE ON servers
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_sessions AFTER UPDATE OF revoked_at ON sessions
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_join_tokens AFTER INSERT OR UPDATE OF used_at OR DELETE ON join_tokens
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_link_tokens AFTER INSERT OR UPDATE OF used_at, revoked_at OR DELETE ON link_tokens
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
