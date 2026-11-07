// SPDX-License-Identifier: AGPL-3.0-only

import { type Backup, BackupMethod, BackupState } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useCallback } from "react";

/** How a backup's state reads. */
export function backupStatus(backup: Backup): Status {
  switch (backup.state) {
    case BackupState.DONE:
      return backup.method === BackupMethod.COPY_RUNNING ? "degraded" : "healthy";
    case BackupState.FAILED:
      return "down";
    case BackupState.RUNNING:
      return "deploying";
  }
  return "unknown";
}

/** Words for how a backup was taken, which say what it's worth. */
export function useMethodWord(): (method: BackupMethod, database: string) => string {
  const { t } = useLingui();
  return useCallback(
    (method, database) => {
      switch (method) {
        case BackupMethod.DUMP:
          return t`${database} dump`;
        case BackupMethod.COPY:
          return t`Exact copy`;
        case BackupMethod.COPY_RUNNING:
          return t`Copy of a running app`;
      }
      return "";
    },
    [t],
  );
}

/** A backup's state, in a word. */
export function useStateWord(): (backup: Backup) => string {
  const { t } = useLingui();
  return useCallback(
    (backup) => {
      switch (backup.state) {
        case BackupState.DONE:
          return t`Done`;
        case BackupState.FAILED:
          return t`Failed`;
        case BackupState.RUNNING:
          return t`Running`;
      }
      return t`Unknown`;
    },
    [t],
  );
}
