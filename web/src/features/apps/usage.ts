// SPDX-License-Identifier: AGPL-3.0-only

import type { ServiceInfo } from "@dockwell/api";

import { useFleet } from "../servers/useFleet";

/** What an app used over its servers' recent samples, every replica together (#797). */
export interface AppUsageSeries {
  /** Its CPU at each sample, as a share of one CPU, oldest first; null where it had nothing running. */
  cpu: readonly (number | null)[];
  /** Its memory in bytes at each sample, as cpu. */
  memory: readonly (number | null)[];
  /** The newest sample's CPU and memory. */
  cpuNow: number;
  memoryNow: number;
  /** Its memory limit, summed over its replicas, or 0 with none. */
  memoryLimit: number;
}

/**
 * An app's CPU and memory over the samples its servers keep, 5 minutes of them,
 * summed over every container it runs on every server it's on, each server's
 * samples lined up from the newest (#797). Undefined while none of its servers'
 * samples name any of its containers: an agent from before this, a host without
 * cgroup v2, or nothing of it running.
 */
export function useAppUsage(service: ServiceInfo): AppUsageSeries | undefined {
  const fleet = useFleet();
  const usage = fleet.status === "connecting" ? undefined : fleet.usage;
  if (!usage) return undefined;
  const servers = [...new Set(service.deployments.map((d) => d.serverId))];
  const perServer = servers.map((id) =>
    (usage.get(id) ?? []).map((sample) => {
      const mine = sample.containers.filter((c) => c.serviceId === service.id);
      if (mine.length === 0) return undefined;
      return {
        cpu: mine.reduce((sum, c) => sum + c.cpuPercent, 0),
        memory: mine.reduce((sum, c) => sum + Number(c.memoryBytes), 0),
        limit: mine.every((c) => c.memoryLimitBytes > 0n)
          ? mine.reduce((sum, c) => sum + Number(c.memoryLimitBytes), 0)
          : 0,
      };
    }),
  );
  if (!perServer.some((samples) => samples.some((s) => s !== undefined))) return undefined;
  const length = Math.max(...perServer.map((samples) => samples.length));
  const cpu: (number | null)[] = [];
  const memory: (number | null)[] = [];
  let limit = 0;
  for (let back = length - 1; back >= 0; back--) {
    let c: number | null = null;
    let m: number | null = null;
    for (const samples of perServer) {
      const s = samples[samples.length - 1 - back];
      if (!s) continue;
      c = (c ?? 0) + s.cpu;
      m = (m ?? 0) + s.memory;
      if (back === 0) limit += s.limit;
    }
    cpu.push(c);
    memory.push(m);
  }
  return { cpu, memory, cpuNow: cpu.at(-1) ?? 0, memoryNow: memory.at(-1) ?? 0, memoryLimit: limit };
}
