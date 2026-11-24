// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type Backup,
  BackupCopy,
  BackupService,
  BackupState,
  DrillResult,
  type ListBackupsResponse,
  ResourceService,
} from "@dockwell/api";
import { type InfiniteData, useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef } from "react";

import { usePlaces } from "../apps/useServices";
import { useSignedIn } from "../auth/useAuthState";
import { LIST_PAGE_SIZE } from "../shell/list";

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

type History = InfiniteData<ListBackupsResponse, string>;
function useHistoryKey() {
  const auth = useSignedIn();
  return useMemo(
    () =>
      [
        "connect-query",
        {
          serviceName: BackupService.typeName,
          methodName: "BackupHistory",
          user: auth?.user?.id,
          grants: auth?.grants.map(({ projectId, role }) => [projectId, role]),
        },
      ] as const,
    [auth],
  );
}

function behind(backup: Backup, tail: Backup) {
  if (!backup.startedAt || !tail.startedAt) return false;
  const at = timestampMs(backup.startedAt);
  const end = timestampMs(tail.startedAt);
  return at < end || (at === end && backup.id < tail.id);
}

/** The fleet reads older pages on demand. Only the newest page polls while work is under way. */
export function useBackupPages() {
  const transport = useTransport();
  const cache = useQueryClient();
  const historyKey = useHistoryKey();
  const newest = useQuery(
    BackupService.method.listBackups,
    { pageSize: LIST_PAGE_SIZE },
    {
      refetchInterval: (query) => (query.state.data?.backups.some(underWay) ? WHILE_RUNNING : false),
    },
  );
  const history = useInfiniteQuery({
    queryKey: historyKey,
    queryFn: ({ pageParam, signal }) =>
      createClient(BackupService, transport).listBackups(
        { pageSize: LIST_PAGE_SIZE, pageToken: pageParam },
        { signal },
      ),
    initialPageParam: "",
    getNextPageParam: (page, _pages, _param, params) =>
      page.nextPageToken && !params.includes(page.nextPageToken) ? page.nextPageToken : undefined,
    enabled: false,
  });
  const fetchOlder = history.fetchNextPage;
  const first = newest.data;
  const fetchNextPage = useCallback(() => {
    if (!first?.nextPageToken) return;
    // Anchor the history on its first requested cursor. New backups can shift the
    // live first page while that cursor still points past the original boundary.
    cache.setQueryData<History>(historyKey, (data) => data ?? { pages: [first], pageParams: [""] });
    return fetchOlder({ cancelRefetch: false });
  }, [cache, first, fetchOlder, historyKey]);
  const previous = useRef(first);
  useEffect(() => {
    const before = previous.current;
    previous.current = first;
    const tail = first?.backups.at(-1);
    if (!before || !tail || !first?.nextPageToken) return;
    const shifted = before.backups.filter((b) => behind(b, tail));
    if (!shifted.length) return;
    cache.setQueryData<History>(historyKey, (data) => {
      const anchor = data?.pages[0];
      if (!data || !anchor) return data;
      const seen = new Set(anchor.backups.map((b) => b.id));
      const added = shifted.filter((b) => !seen.has(b.id));
      if (!added.length) return data;
      return { ...data, pages: [{ ...anchor, backups: [...added, ...anchor.backups] }, ...data.pages.slice(1)] };
    });
  }, [cache, first, historyKey]);
  const backups = useMemo(() => {
    if (!first) return [];
    const tail = first.backups.at(-1);
    if (!first.nextPageToken || !tail?.startedAt) {
      return first.backups.length === 0 && first.nextPageToken && history.data
        ? history.data.pages.slice(1).flatMap((page) => page.backups)
        : first.backups;
    }
    const ids = new Set(first.backups.map((b) => b.id));
    const all = [...first.backups];
    for (const page of history.data?.pages ?? []) {
      for (const backup of page.backups) {
        if (ids.has(backup.id) || !behind(backup, tail)) continue;
        ids.add(backup.id);
        all.push(backup);
      }
    }
    return all;
  }, [first, history.data]);
  return {
    backups,
    fetchNextPage,
    hasNextPage: !!first?.nextPageToken && (!history.data || history.hasNextPage),
    isFetching: newest.isFetching || history.isFetching,
    isPending: newest.isPending,
    isFetchingNextPage: history.isFetchingNextPage,
    isFetchNextPageError: history.isFetchNextPageError,
    error: history.error ?? newest.error,
    refetch: newest.refetch,
  };
}

/** A change to one old backup reads or removes that receipt without refreshing its neighbours. */
export function useRefreshOlderBackup() {
  const transport = useTransport();
  const cache = useQueryClient();
  const historyKey = useHistoryKey();
  return async (id: string, deleted = false) => {
    if (!cache.getQueryData<History>(historyKey)?.pages.some((page) => page.backups.some((b) => b.id === id))) return;
    const updated = deleted
      ? undefined
      : await createClient(BackupService, transport)
          .getBackup({ id })
          .then(({ backup }) => backup)
          .catch(() => undefined);
    if (!deleted && !updated) return;
    cache.setQueryData<History>(
      historyKey,
      (data) =>
        data && {
          ...data,
          pages: data.pages.map((page) => ({
            ...page,
            backups: page.backups.flatMap((b) => (b.id === id ? (updated ? [updated] : []) : [b])),
          })),
        },
    );
  };
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
