// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type Backup, BackupCopy, BackupService, BackupState, DrillResult, ResourceService } from "@dockwell/api";
import { useMemo } from "react";

import { usePlaces } from "../apps/useServices";

/** How often a list with a backup under way is read again. */
const WHILE_RUNNING = 2_000;

/** The most a list reads. A volume's own are few, and the page shows the newest. */
const PAGE_SIZE = 200;

/** The backups, newest first, of one volume or all of them. */
/** Something is still happening to a backup: it runs, it's on its way to storage, it's coming back, or a drill checks it. */
function underWay(b: Backup) {
  return (
    b.state === BackupState.RUNNING ||
    b.copy === BackupCopy.PENDING ||
    b.progress !== "" ||
    b.drill?.result === DrillResult.RUNNING
  );
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

/** Every volume there is, by name, with whether that's known yet and why not. */
export function useVolumesRead(): {
  volumes: VolumeChoice[];
  isPending: boolean;
  isFetching: boolean;
  error: Error | null;
  refetch: () => void;
} {
  const query = useQuery(ResourceService.method.listResources, { kind: "volume" });
  const resources = query.data?.resources;
  const volumes = useMemo(
    () =>
      (resources ?? [])
        .map((r) => ({ id: r.id, name: r.resource?.name ?? r.id, environment: r.resource?.scope ?? "" }))
        .sort((a, b) => a.name.localeCompare(b.name)),
    [resources],
  );
  return {
    volumes,
    isPending: query.isPending,
    isFetching: query.isFetching,
    error: query.error,
    refetch: () => void query.refetch(),
  };
}

/** Every volume there is, by name: none while they load or can't be read. */
export function useVolumes(): VolumeChoice[] {
  return useVolumesRead().volumes;
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
