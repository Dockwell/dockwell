// SPDX-License-Identifier: AGPL-3.0-only

import { AlertRule_Condition as Condition } from "@dockwell/api";
import { msg } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";

const LABELS = {
  [Condition.SERVICE_DOWN]: msg`App down`,
  [Condition.CHECK_DOWN]: msg`Uptime check`,
  [Condition.SERVER_UNREACHABLE]: msg`Server offline`,
  [Condition.DISK_FULL]: msg`Disk full`,
  [Condition.MEMORY_HIGH]: msg`High memory use`,
  [Condition.CERTIFICATE_EXPIRING]: msg`Certificate expiring`,
  [Condition.CERTIFICATE_FAILED]: msg`Certificate renewal failed`,
  [Condition.DOMAIN_UNPUBLISHED]: msg`Domain unpublished`,
  [Condition.BACKUP_FAILED]: msg`Backup failed`,
  [Condition.BACKUP_LATE]: msg`Backup late`,
  [Condition.RESTORE_DRILL_FAILED]: msg`Restore test failed`,
  [Condition.RESTORE_DRILL_LATE]: msg`Restore test late`,
  [Condition.CHANGE_FAILED]: msg`Change failed`,
  [Condition.UPDATE_AVAILABLE]: msg`Update available`,
  [Condition.CATALOG_SOURCE_STUCK]: msg`Catalog source stalled`,
  [Condition.UNSPECIFIED]: msg`Alert`,
};

export function useAlertKind() {
  const { i18n } = useLingui();
  return (condition: Condition) => i18n._(LABELS[condition] ?? LABELS[Condition.UNSPECIFIED]);
}
