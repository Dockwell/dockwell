// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { BackupService } from "@dockwell/api";

import { latestDone, useBackups } from "../backups/useBackups";
import { useMethodWord } from "../backups/words";

export function useBackupRows(id: string, serverId: string) {
  const list = useBackups(id);
  const preview = useQuery(BackupService.method.previewBackup, { volumeId: id, server: serverId }, { retry: false });
  const method = useMethodWord();
  return {
    backup: latestDone(
      (list.data?.backups ?? []).filter((backup) => backup.serverId === serverId),
      id,
    ),
    method: preview.data ? method(preview.data.method, preview.data.database) : undefined,
    pending: preview.isPending || list.isPending,
  };
}
