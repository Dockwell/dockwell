-- SPDX-License-Identifier: AGPL-3.0-only

CREATE TRIGGER control_backup_ci_deploy_keys AFTER INSERT OR UPDATE OR DELETE ON ci_deploy_keys
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
CREATE TRIGGER control_backup_ci_deploy_origins AFTER INSERT OR UPDATE OR DELETE ON ci_deploy_origins
  FOR EACH ROW EXECUTE FUNCTION control_backup_changed();
