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
import { kindOf, targetOf, useWebsites } from "../websites/website";

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

/** The backups, newest first, of one website's folder. */
export function useWebsiteBackups(websiteId: string) {
  return useQuery(
    BackupService.method.listBackups,
    { pageSize: 20, websiteId },
    {
      enabled: websiteId !== "",
      refetchInterval: (query) => (query.state.data?.backups.some(underWay) ? WHILE_RUNNING : false),
    },
  );
}

type HistoryPageParam = { token: string; size: number };
type History = InfiniteData<ListBackupsResponse, HistoryPageParam>;
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
export function useBackupPages(searchingAll = false) {
  const transport = useTransport();
  const cache = useQueryClient();
  const historyKey = useHistoryKey();
  const searchingAllRef = useRef(searchingAll);
  searchingAllRef.current = searchingAll;
  const pageSizesRef = useRef<number[]>([]);
  const newest = useQuery(
    BackupService.method.listBackups,
    { pageSize: LIST_PAGE_SIZE },
    {
      refetchInterval: (query) => (query.state.data?.backups.some(underWay) ? WHILE_RUNNING : false),
      refetchOnWindowFocus: () => !searchingAllRef.current,
    },
  );
  const firstRef = useRef(newest.data);
  firstRef.current = newest.data;
  const history = useInfiniteQuery({
    queryKey: historyKey,
    queryFn: ({ pageParam, signal }) => {
      if (pageParam.token === "" && firstRef.current) return Promise.resolve(firstRef.current);
      return createClient(BackupService, transport).listBackups(
        { pageSize: pageParam.size, pageToken: pageParam.token },
        { signal },
      );
    },
    initialPageParam: { token: "", size: LIST_PAGE_SIZE } as HistoryPageParam,
    getNextPageParam: (page, pages, _param, params) => {
      if (!page.nextPageToken || params.some((p) => p.token === page.nextPageToken)) return undefined;
      const cachedSize = cache.getQueryData<History>(historyKey)?.pageParams[pages.length]?.size;
      const size =
        pageSizesRef.current[pages.length] ?? cachedSize ?? (searchingAllRef.current ? PAGE_SIZE : LIST_PAGE_SIZE);
      pageSizesRef.current[pages.length] = size;
      return { token: page.nextPageToken, size };
    },
    enabled: false,
  });
  const fetchOlder = history.fetchNextPage;
  const refetchHistory = history.refetch;
  const first = newest.data;
  const fetchNextPage = useCallback(() => {
    if (!first?.nextPageToken) return;
    // Anchor the history on its first requested cursor. New backups can shift the
    // live first page while that cursor still points past the original boundary.
    cache.setQueryData<History>(
      historyKey,
      (data) => data ?? { pages: [first], pageParams: [{ token: "", size: LIST_PAGE_SIZE }] },
    );
    return fetchOlder({ cancelRefetch: false });
  }, [cache, first, fetchOlder, historyKey]);
  const previous = useRef(first);
  const previousUpdatedAt = useRef(newest.dataUpdatedAt);
  useEffect(() => {
    const before = previous.current;
    const beforeUpdatedAt = previousUpdatedAt.current;
    previous.current = first;
    previousUpdatedAt.current = newest.dataUpdatedAt;
    if (!before || !first || newest.dataUpdatedAt === beforeUpdatedAt) return;
    const existing = cache.getQueryData<History>(historyKey);
    if (!existing) return;
    const wasRunning = before.backups.some(underWay);
    const stillRunning = first.backups.some(underWay);
    // Keep polling lightweight while a backup is actively running, and skip
    // re-reading loaded 200-row pages on window refocus during Search all backups.
    if ((wasRunning && stillRunning) || searchingAllRef.current) {
      const tail = first.backups.at(-1);
      if (!tail || !first.nextPageToken) return;
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
      return;
    }
    // Outside active polling (invalidation, manual refresh, refocus, or a
    // running backup finishing and potentially pruning old backups), refresh
    // loaded older pages at their original sizes so pruned backups leave the screen.
    if (!first.nextPageToken) {
      pageSizesRef.current = [];
      cache.removeQueries({ queryKey: historyKey, exact: true });
      return;
    }
    void refetchHistory();
  }, [cache, first, historyKey, newest.dataUpdatedAt, refetchHistory]);
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
  const refetch = useCallback(async () => {
    const res = await newest.refetch();
    if (cache.getQueryData<History>(historyKey)) await refetchHistory();
    return res;
  }, [cache, historyKey, newest, refetchHistory]);
  return {
    backups,
    fetchNextPage,
    hasNextPage: !!first?.nextPageToken && (!history.data || history.hasNextPage),
    isFetching: newest.isFetching || history.isFetching,
    isPending: newest.isPending,
    isFetchingNextPage: history.isFetchingNextPage,
    isFetchNextPageError: history.isFetchNextPageError,
    error: history.error ?? newest.error,
    refetch,
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

/** A volume or website folder someone can back up. */
export interface VolumeChoice {
  id: string;
  name: string;
  environment: string;
  /** Set when this target is a website's folder rather than a Docker volume. */
  websiteId?: string;
  websitePath?: string;
}

/** Every volume and folder/PHP website there is, by name, with whether that's known yet and why not. */
export function useVolumesRead(): {
  volumes: VolumeChoice[];
  isPending: boolean;
  isFetching: boolean;
  error: Error | null;
  refetch: () => void;
} {
  const query = useQuery(ResourceService.method.listResources, { kind: "volume" });
  const websites = useWebsites();
  const resources = query.data?.resources;
  const siteList = websites.data;
  const volumes = useMemo(() => {
    const out: VolumeChoice[] = (resources ?? []).map((r) => ({
      id: r.id,
      name: r.resource?.name ?? r.id,
      environment: r.resource?.scope ?? "",
    }));
    for (const w of siteList ?? []) {
      const kind = kindOf(w.spec);
      if (kind !== "folder" && kind !== "php") continue;
      out.push({
        id: w.id,
        name: w.domain || w.name,
        environment: w.environmentId,
        websiteId: w.id,
        websitePath: targetOf(w.spec),
      });
    }
    return out.sort((a, b) => a.name.localeCompare(b.name));
  }, [resources, siteList]);
  return {
    volumes,
    isPending: query.isPending,
    isFetching: query.isFetching,
    error: query.error,
    refetch: () => {
      void query.refetch();
      void websites.refetch();
    },
  };
}

/** Every volume there is, by name: none while they load or can't be read. */
export function useVolumes(): VolumeChoice[] {
  return useVolumesRead().volumes;
}

/** What a list of volumes and website folders offers: each by its name, and where that's not enough, its app too. */
export function useVolumeOptions(volumes: readonly VolumeChoice[]): { value: string; label: string }[] {
  const places = usePlaces();
  return volumes.map((v) => {
    if (v.websiteId) {
      return { value: v.id, label: v.websitePath ? `${v.name} (${v.websitePath})` : v.name };
    }
    const shared = volumes.some((other) => other.id !== v.id && other.name === v.name);
    const place = places.label(v.environment);
    return { value: v.id, label: shared && place ? `${v.name} (${place})` : v.name };
  });
}

/** A volume's newest backup that's whole. */
export function latestDone(backups: readonly Backup[], volumeId: string): Backup | undefined {
  return backups.find((b) => b.volumeId === volumeId && b.state === BackupState.DONE);
}
