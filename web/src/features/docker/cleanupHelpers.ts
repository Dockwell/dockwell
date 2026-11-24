// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import {
  DockerCleanupKind,
  DockerCleanupReason,
  type DockerCleanupServerLimits,
  type DockerCleanupTarget,
  DockerReadState,
  DockerService,
  type Server,
  ServerService,
} from "@dockwell/api";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo } from "react";

export type CleanupServer = Pick<Server, "id" | "name">;

export interface CleanupFilterSpec {
  taggedImages: boolean;
  networks: boolean;
  volumes: boolean;
  kinds: readonly DockerCleanupKind[];
}

const DEFAULT_SERVER_CAP = 200;
const DEFAULT_TOTAL_CAP = 500;
const HEX64_RE = /^[0-9a-f]{64}$/;

export function isHex64VolumeName(name: string): boolean {
  return HEX64_RE.test(name);
}

export function shortAnonymousVolumeName(name: string): string {
  return isHex64VolumeName(name) ? name.slice(0, 12) : name;
}

export function cleanupTargetKey(target: DockerCleanupTarget): string {
  return `${target.serverId}/${target.kind}/${target.engineId}`;
}

export function cleanupTargetFullName(target: DockerCleanupTarget, fallback = ""): string {
  if (target.kind === DockerCleanupKind.IMAGE && target.imageTags.length === 0) {
    if (target.name && target.name !== target.engineId) return target.name;
    return target.engineId;
  }
  return target.name || target.engineId || fallback;
}

export function isAnonymousVolumeTarget(target: DockerCleanupTarget): boolean {
  return (
    target.kind === DockerCleanupKind.VOLUME &&
    (target.reason === DockerCleanupReason.ANONYMOUS || isHex64VolumeName(target.name))
  );
}

export function cleanupTargetDisplayName(target: DockerCleanupTarget, fallback = ""): string {
  if (target.kind === DockerCleanupKind.IMAGE && target.imageTags.length === 0) {
    if (target.name && target.name !== target.engineId) return target.name;
    return target.engineId.slice(7, 19);
  }
  if (target.kind === DockerCleanupKind.VOLUME && isHex64VolumeName(target.name)) {
    return shortAnonymousVolumeName(target.name);
  }
  return target.name || target.engineId || fallback;
}

export function isOptInTaggedImage(target: DockerCleanupTarget): boolean {
  return target.kind === DockerCleanupKind.IMAGE && target.reason === DockerCleanupReason.UNUSED;
}

export function isOptInNamedVolume(target: DockerCleanupTarget): boolean {
  return target.kind === DockerCleanupKind.VOLUME && target.reason === DockerCleanupReason.UNUSED;
}

export function isSafeDefaultTarget(target: DockerCleanupTarget): boolean {
  if (target.evidence === "") return false;
  return !isOptInTaggedImage(target) && !isOptInNamedVolume(target);
}

function targetPriority(target: DockerCleanupTarget): number {
  switch (target.kind) {
    case DockerCleanupKind.IMAGE:
      return target.reason === DockerCleanupReason.DANGLING ? 0 : 4;
    case DockerCleanupKind.BUILD_CACHE:
      return 1;
    case DockerCleanupKind.NETWORK:
      return 2;
    case DockerCleanupKind.VOLUME:
      return target.reason === DockerCleanupReason.ANONYMOUS ? 3 : 5;
    default:
      return 6;
  }
}

export function sortTargetsForBatch(targets: readonly DockerCleanupTarget[]): DockerCleanupTarget[] {
  return [...targets].sort((a, b) => {
    const pa = targetPriority(a);
    const pb = targetPriority(b);
    if (pa !== pb) return pa - pb;
    const ba = a.estimatedBytes ?? 0n;
    const bb = b.estimatedBytes ?? 0n;
    if (ba !== bb) return ba > bb ? -1 : 1;
    if (a.serverId !== b.serverId) return a.serverId.localeCompare(b.serverId);
    return a.engineId.localeCompare(b.engineId);
  });
}

export interface BoundedSelectionResult {
  defaultSelectedKeys: ReadonlySet<string>;
  deferredDefaultCount: number;
  maxTargetsTotal: number;
  limitByServer: ReadonlyMap<string, number>;
  unsupportedVolumeServers: readonly string[];
}

/**
 * Computes bounded default selections respecting every server's effective
 * target cap and the fleet cap. Selects safest targets first, then largest.
 */
export function computeBoundedDefaults(
  candidates: readonly DockerCleanupTarget[],
  serverLimits: readonly DockerCleanupServerLimits[],
  maxTargetsTotal: number,
  focusEngineId?: string,
): BoundedSelectionResult {
  const totalCap = maxTargetsTotal > 0 ? maxTargetsTotal : DEFAULT_TOTAL_CAP;
  const limitByServer = new Map<string, number>();
  const unsupportedVolumeServers: string[] = [];
  for (const row of serverLimits) {
    if (row.maxTargets > 0) {
      limitByServer.set(row.serverId, row.maxTargets);
    }
    if (row.supportedKinds.length > 0 && !row.supportedKinds.includes(DockerCleanupKind.VOLUME)) {
      unsupportedVolumeServers.push(row.serverId);
    }
  }

  const eligibleDefaults = focusEngineId
    ? candidates.filter((row) => row.engineId === focusEngineId)
    : candidates.filter(isSafeDefaultTarget);

  const selectedKeys = new Set<string>();
  const perServer = new Map<string, number>();
  let deferredDefaultCount = 0;

  for (const target of sortTargetsForBatch(eligibleDefaults)) {
    const serverCap = limitByServer.get(target.serverId) ?? DEFAULT_SERVER_CAP;
    const usedOnServer = perServer.get(target.serverId) ?? 0;
    if (selectedKeys.size >= totalCap || usedOnServer >= serverCap) {
      deferredDefaultCount++;
      continue;
    }
    selectedKeys.add(cleanupTargetKey(target));
    perServer.set(target.serverId, usedOnServer + 1);
  }

  return {
    defaultSelectedKeys: selectedKeys,
    deferredDefaultCount,
    maxTargetsTotal: totalCap,
    limitByServer,
    unsupportedVolumeServers,
  };
}

/**
 * Applies user checkbox overrides while guaranteeing the final selection never
 * exceeds any server's limit or the fleet limit.
 */
export function resolveBoundedSelection(
  candidates: readonly DockerCleanupTarget[],
  bounded: BoundedSelectionResult,
  overrides: ReadonlyMap<string, boolean>,
): {
  selected: DockerCleanupTarget[];
  selectedKeys: ReadonlySet<string>;
  deferredCount: number;
  canSelectTarget: (target: DockerCleanupTarget) => boolean;
} {
  const wanted = candidates.filter((target) => {
    const key = cleanupTargetKey(target);
    const override = overrides.get(key);
    return override !== undefined ? override : bounded.defaultSelectedKeys.has(key);
  });

  const selected: DockerCleanupTarget[] = [];
  const selectedKeys = new Set<string>();
  const perServer = new Map<string, number>();
  let deferredCount = 0;

  for (const target of sortTargetsForBatch(wanted)) {
    const serverCap = bounded.limitByServer.get(target.serverId) ?? DEFAULT_SERVER_CAP;
    const usedOnServer = perServer.get(target.serverId) ?? 0;
    if (selected.length >= bounded.maxTargetsTotal || usedOnServer >= serverCap) {
      deferredCount++;
      continue;
    }
    selected.push(target);
    selectedKeys.add(cleanupTargetKey(target));
    perServer.set(target.serverId, usedOnServer + 1);
  }

  // Also include safe defaults that didn't fit when the user hasn't explicitly unchecked them.
  const remainingSafeDefaults = candidates.filter((target) => {
    const key = cleanupTargetKey(target);
    if (selectedKeys.has(key)) return false;
    if (overrides.get(key) === false) return false;
    return isSafeDefaultTarget(target);
  }).length;

  const canSelectTarget = (target: DockerCleanupTarget) => {
    const key = cleanupTargetKey(target);
    if (selectedKeys.has(key)) return true;
    const serverCap = bounded.limitByServer.get(target.serverId) ?? DEFAULT_SERVER_CAP;
    const usedOnServer = perServer.get(target.serverId) ?? 0;
    return selected.length < bounded.maxTargetsTotal && usedOnServer < serverCap;
  };

  return {
    selected,
    selectedKeys,
    deferredCount: Math.max(deferredCount, remainingSafeDefaults),
    canSelectTarget,
  };
}

/**
 * Fetches every page of a Docker cleanup preview so the person never hits a
 * page or item ceiling, while keeping already-arrived servers and pages visible
 * during measuring or pagination (Loading principle 7).
 */
export function useCleanupPreview(servers: readonly CleanupServer[], filters: CleanupFilterSpec, enabled = true) {
  const transport = useTransport();
  const client = useMemo(() => createClient(DockerService, transport), [transport]);
  const cache = useQueryClient();
  const serverIds = useMemo(() => servers.map((s) => s.id), [servers]);
  const queryKey = useMemo(() => ["docker-cleanup", serverIds, filters] as const, [serverIds, filters]);
  const query = useInfiniteQuery({
    queryKey,
    enabled: enabled && serverIds.length > 0,
    queryFn: ({ pageParam, signal }) =>
      client.previewDockerCleanup(
        {
          serverIds,
          filters: {
            taggedImages: filters.taggedImages,
            networks: filters.networks,
            volumes: filters.volumes,
            kinds: [...filters.kinds],
          },
          pageSize: 250,
          pageToken: pageParam,
        },
        { signal },
      ),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextPageToken || undefined,
    placeholderData: (previous) => previous,
    refetchOnMount: "always",
    refetchOnWindowFocus: false,
    refetchInterval: ({ state }) =>
      state.data?.pages[0]?.unavailable.some((row) => row.state === DockerReadState.UNSPECIFIED) ? 2_000 : false,
    retry: false,
  });

  const { hasNextPage, isFetching, isError, fetchNextPage } = query;
  useEffect(() => {
    if (enabled && hasNextPage && !isFetching && !isError) {
      void fetchNextPage();
    }
  }, [enabled, hasNextPage, isFetching, isError, fetchNextPage]);

  const pages = query.data?.pages ?? [];
  const pauses = pages[0]?.pauses ?? [];
  const unavailable = pages[0]?.unavailable ?? [];
  const serverLimits = pages[0]?.serverLimits ?? [];
  const maxTargetsTotal = pages[0]?.maxTargetsTotal ?? 0;
  const measuringServers = unavailable.filter((row) => row.state === DockerReadState.UNSPECIFIED);
  const measuring = measuringServers.length > 0;
  const candidates = useMemo(() => pages.flatMap((page) => page.candidates), [pages]);
  const protectedRows = useMemo(() => pages.flatMap((page) => page.exclusions), [pages]);
  const snapshot = pages[0]?.snapshot ?? "";
  const loadingMore = query.isRefetching || query.isFetchingNextPage || query.hasNextPage;
  const hasAnyData =
    candidates.length > 0 ||
    protectedRows.length > 0 ||
    pauses.length > 0 ||
    unavailable.some((row) => row.state !== DockerReadState.UNSPECIFIED);

  const reviewAgain = () => {
    // Refetch rebuilds the page chain from its first page while keeping the current rows visible.
    void query.refetch();
  };

  const invalidateAfterCleanup = () => {
    reviewAgain();
    void cache.invalidateQueries({ queryKey: ["docker-cleanup"] });
    void cache.invalidateQueries({
      queryKey: createConnectQueryKey({ schema: DockerService.method.listDockerObjects, cardinality: undefined }),
    });
    void cache.invalidateQueries({
      queryKey: createConnectQueryKey({ schema: DockerService.method.getDockerObject, cardinality: undefined }),
    });
    void cache.invalidateQueries({
      queryKey: createConnectQueryKey({ schema: DockerService.method.getDockerOverview, cardinality: undefined }),
    });
    void cache.invalidateQueries({
      queryKey: createConnectQueryKey({ schema: ServerService.method.getServerDiskUse, cardinality: undefined }),
    });
  };

  return {
    client,
    query,
    serverIds,
    snapshot,
    pauses,
    unavailable,
    measuringServers,
    measuring,
    serverLimits,
    maxTargetsTotal,
    candidates,
    protectedRows,
    loadingMore,
    hasAnyData,
    reviewAgain,
    invalidateAfterCleanup,
  };
}
