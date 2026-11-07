// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type Backup, BackupCopy, BackupService, BackupState, ResourceService } from "@dockwell/api";
import { useMemo } from "react";

import { usePlaces } from "../apps/useServices";

/** How often a list with a backup under way is read again. */
const WHILE_RUNNING = 2_000;

/** The most a list reads. A volume's own are few, and the page shows the newest. */
const PAGE_SIZE = 200;

/** The backups, newest first, of one volume or all of them. */
/** Something is still happening to a backup: it runs, it's on its way to storage, or it's coming back. */
function underWay(b: Backup) {
  return b.state === BackupState.RUNNING || b.copy === BackupCopy.PENDING || b.progress !== "";
}

export function useBackups(volumeId = "") {
  return useQuery(
    BackupService.method.listBackups,
    { pageSize: PAGE_SIZE, volumeId },
    {
      refetchInterval: (query) => (query.state.data?.backups.some(underWay) ? WHILE_RUNNING : false),
    },
  );
}

/** A volume someone can back up. */
export interface VolumeChoice {
  id: string;
  name: string;
  environment: string;
}

/** Every volume there is, by name. */
export function useVolumes(): VolumeChoice[] {
  const query = useQuery(ResourceService.method.listResources, { kind: "volume" });
  const resources = query.data?.resources;
  return useMemo(
    () =>
      (resources ?? [])
        .map((r) => ({ id: r.id, name: r.resource?.name ?? r.id, environment: r.resource?.scope ?? "" }))
        .sort((a, b) => a.name.localeCompare(b.name)),
    [resources],
  );
}

/** What a list of volumes offers: each by its name, and where that's not enough, its app too. */
export function useVolumeOptions(volumes: readonly VolumeChoice[]): { value: string; label: string }[] {
  const places = usePlaces();
  return volumes.map((v) => {
    const shared = volumes.some((other) => other.id !== v.id && other.name === v.name);
    const place = places.label(v.environment);
    return { value: v.id, label: shared && place ? `${v.name} (${place})` : v.name };
  });
}

/** A volume's newest backup that's whole. */
export function latestDone(backups: readonly Backup[], volumeId: string): Backup | undefined {
  return backups.find((b) => b.volumeId === volumeId && b.state === BackupState.DONE);
}
