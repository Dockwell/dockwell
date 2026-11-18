// SPDX-License-Identifier: AGPL-3.0-only

import { Connection, type ServiceInfo } from "@dockwell/api";

import { useFleet, useFleetUnread } from "../servers/useFleet";

/** What an app used over its servers' recent samples, every replica together (#797). */
export interface AppUsageSeries {
  /** Its CPU at each sample, as a share of one CPU, summed over its replicas, oldest first; null where it had nothing running. */
  cpu: readonly (number | null)[];
  /** Its memory in bytes at each sample, as cpu. */
  memory: readonly (number | null)[];
  /** How many replicas it ran at each sample, as cpu, which a CPU limit for one is multiplied by. */
  replicas: readonly (number | null)[];
  /** The newest sample's CPU, memory and replicas. */
  cpuNow: number;
  memoryNow: number;
  replicasNow: number;
  /** Its memory limit, summed over its replicas, or 0 where any has none. */
  memoryLimit: number;
  /** How many of the servers it's on aren't reporting, whose use is left out (#852's review, L3). */
  missing: number;
  /**
   * Whether what the panel last heard still stands: not while its watch of the
   * fleet reconnects, or has heard nothing for too long (#852's review, L4).
   */
  current: boolean;
}

type Point = { cpu: number; memory: number; replicas: number; limit: number };

/**
 * An app's CPU and memory over the samples its servers keep, 5 minutes of them,
 * summed over every replica on every server it's on, each server's samples lined
 * up from the newest (#797). Undefined while none of its servers' newest samples
 * have any of it running, so a card or a hint never shows figures from before it
 * stopped (#848's review, M1), and while no sample has any of it: an agent from
 * before this, a host without cgroup v2, or nothing of it running. A server that
 * doesn't read connected counts as running none of it: it sends no more samples,
 * so its last one would stay its newest (#852).
 */
export function useAppUsage(service: ServiceInfo): AppUsageSeries | undefined {
  const fleet = useFleet();
  const unread = useFleetUnread();
  if (fleet.status === "connecting" || !fleet.usage) return undefined;
  const usage = fleet.usage;
  const connected = new Set(fleet.servers.filter((s) => s.connection === Connection.CONNECTED).map((s) => s.id));
  const on = [...new Set(service.deployments.map((d) => d.serverId))];
  const servers = on.filter((id) => connected.has(id));
  const perServer = servers.map((id) =>
    (usage.get(id) ?? []).map((sample): Point | undefined => {
      const mine = sample.containers.find((c) => c.serviceId === service.id);
      if (!mine) return undefined;
      return {
        cpu: mine.cpuPercent,
        memory: Number(mine.memoryBytes),
        replicas: Math.max(1, mine.replicas),
        limit: Number(mine.memoryLimitBytes),
      };
    }),
  );
  // The newest sample of each server it's on says whether it runs now.
  const newest = perServer.map((samples) => samples.at(-1));
  if (!newest.some((p) => p !== undefined)) return undefined;
  const length = Math.max(...perServer.map((samples) => samples.length));
  const cpu: (number | null)[] = [];
  const memory: (number | null)[] = [];
  const replicas: (number | null)[] = [];
  for (let back = length - 1; back >= 0; back--) {
    let point: Point | undefined;
    for (const samples of perServer) {
      const s = samples[samples.length - 1 - back];
      if (!s) continue;
      point = point
        ? { cpu: point.cpu + s.cpu, memory: point.memory + s.memory, replicas: point.replicas + s.replicas, limit: 0 }
        : { ...s };
    }
    cpu.push(point?.cpu ?? null);
    memory.push(point?.memory ?? null);
    replicas.push(point?.replicas ?? null);
  }
  // A limit for the whole is one where every server's replicas have theirs (L4).
  const running = newest.filter((p): p is Point => p !== undefined);
  const memoryLimit = running.every((p) => p.limit > 0) ? running.reduce((sum, p) => sum + p.limit, 0) : 0;
  return {
    cpu,
    memory,
    replicas,
    cpuNow: cpu.at(-1) ?? 0,
    memoryNow: memory.at(-1) ?? 0,
    replicasNow: replicas.at(-1) ?? 1,
    memoryLimit,
    missing: on.length - servers.length,
    current: fleet.status === "live" && unread === undefined,
  };
}
