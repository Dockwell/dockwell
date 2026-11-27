-- SPDX-License-Identifier: AGPL-3.0-only

CREATE TRIGGER control_backup_git_hooks AFTER INSERT OR UPDATE OR DELETE ON git_hooks
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_github_installations AFTER INSERT OR UPDATE OR DELETE ON github_installations
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_provider_links AFTER INSERT OR UPDATE OR DELETE ON provider_links
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_cloudflare_tunnels AFTER INSERT OR UPDATE OR DELETE ON cloudflare_tunnels
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_cloudflare_records AFTER INSERT OR UPDATE OR DELETE ON cloudflare_records
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_cloudflare_rules AFTER INSERT OR UPDATE OR DELETE ON cloudflare_rules
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_cloudflare_zone_modes AFTER INSERT OR UPDATE OR DELETE ON cloudflare_zone_modes
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_key_encryption_keys AFTER INSERT OR UPDATE OR DELETE ON key_encryption_keys
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_data_keys AFTER INSERT OR UPDATE OR DELETE ON data_keys
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_backups AFTER INSERT OR UPDATE OR DELETE ON backups
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_backup_drills AFTER INSERT OR UPDATE OR DELETE ON backup_drills
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_app_owners AFTER INSERT OR UPDATE OR DELETE ON app_owners
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_alert_mutes AFTER INSERT OR UPDATE OR DELETE ON alert_mutes
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_code_guards AFTER INSERT OR UPDATE OR DELETE ON code_guards
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_pinned_folders AFTER INSERT OR UPDATE OR DELETE ON pinned_folders
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_service_updates AFTER INSERT OR UPDATE OR DELETE ON service_updates
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_change_servers AFTER INSERT OR UPDATE OR DELETE ON change_servers
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_revision_uses AFTER INSERT OR UPDATE OR DELETE ON revision_uses
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
