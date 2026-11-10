// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { Connection, type Server, type ServerStats } from "@dockwell/api";

import { trend as historyTrend, STATS_INTERVAL_MS } from "../servers/history";

/** The servers whose numbers count for the fleet's: those connected now, with a sample. */
export function reporting(servers: readonly Server[]): Server[] {
  return servers.filter((s) => s.connection === Connection.CONNECTED && s.stats !== undefined);
}

/** A server's share of the fleet's CPU: its cores, or one when it doesn't say. */
function cores(server: Server): number {
  return Math.max(server.inventory?.cpuCount ?? 0, 1);
}

/** The fleet's CPU and memory at one time. */
export interface FleetSample {
  /** Busy time across every core of every server, 0 to 100, so a big server counts for more. */
  cpuPercent: number;
  memoryUsedBytes: number;
  memoryTotalBytes: number;
}

/** What the fleet's numbers are now, and how they ran. */
export interface FleetNumbers {
  now: FleetSample;
  /** Oldest first, one a sample interval apart, over the time every server reporting has reported. */
  trend: FleetSample[];
  /** The time the trend covers, one interval per point. 0 for fewer than two. */
  spanMs: number;
  /** Every core the servers reporting have. */
  cpuCount: number;
}

function add(samples: readonly { server: Server; stats: ServerStats }[]): FleetSample {
  let busy = 0;
  let all = 0;
  let used = 0;
  let total = 0;
  for (const { server, stats } of samples) {
    const n = cores(server);
    busy += stats.cpuPercent * n;
    all += n;
    used += Number(stats.memoryUsedBytes);
    total += Number(stats.memoryTotalBytes);
  }
  return { cpuPercent: all > 0 ? busy / all : 0, memoryUsedBytes: used, memoryTotalBytes: total };
}

/** Which sample interval a sample falls in, by its own time. */
function slot(stats: ServerStats): number | undefined {
  return stats.time ? Math.floor(timestampMs(stats.time) / STATS_INTERVAL_MS) : undefined;
}

/**
 * The fleet's CPU and memory, now and over the last few minutes, from what each
 * server reporting has sent since its last gap. A trend point adds up the sample
 * covering that interval on every server, so it starts once every server has
 * data: before that, a point would leave a server out and jump.
 */
export function fleetNumbers(servers: readonly Server[]): FleetNumbers | undefined {
  const live = reporting(servers);
  if (live.length === 0) return undefined;
  const now = add(live.map((server) => ({ server, stats: server.stats as ServerStats })));
  const cpuCount = live.reduce((n, s) => n + (s.inventory?.cpuCount ?? 0), 0);

  const runs = live.map((server) => {
    const samples = historyTrend(server.history.length > 0 ? server.history : [server.stats as ServerStats]).samples;
    const last = slot(samples.at(-1) ?? ({} as ServerStats));
    return { server, samples, first: last === undefined ? undefined : last - samples.length + 1, last };
  });
  // Resampled points can share the original timestamp: a slow sample covers
  // several intervals. Their positions, counted back from the newest, align them.
  const firsts = runs.map((r) => r.first);
  const lasts = runs.map((r) => r.last);
  if (firsts.some((f) => f === undefined) || lasts.some((l) => l === undefined)) {
    return { now, trend: [now], spanMs: 0, cpuCount };
  }
  const from = Math.max(...(firsts as number[]));
  const to = Math.max(...(lasts as number[]));
  const trend: FleetSample[] = [];
  for (let t = from; t <= to; t++) {
    const picked: { server: Server; stats: ServerStats }[] = [];
    runs.forEach((run) => {
      // Keep the newest point while the next one is on its way, as for live values.
      const stats = run.samples[Math.min(t - (run.first as number), run.samples.length - 1)];
      if (stats) picked.push({ server: run.server, stats });
    });
    trend.push(add(picked));
  }
  return { now, trend, spanMs: trend.length > 1 ? trend.length * STATS_INTERVAL_MS : 0, cpuCount };
}

/** How many servers are in each state that matters to the fleet. */
export interface FleetCounts {
  total: number;
  /** Connected, whether or not they can read Docker. */
  serving: number;
  /** Connected, and their agent can't read Docker. */
  degraded: number;
  /** Their agent stopped reporting. */
  down: number;
  /** Joined, and their agent hasn't connected yet. */
  waiting: number;
}

export function fleetCounts(servers: readonly Server[]): FleetCounts {
  const counts: FleetCounts = { total: servers.length, serving: 0, degraded: 0, down: 0, waiting: 0 };
  for (const s of servers) {
    if (s.connection === Connection.CONNECTED) {
      counts.serving++;
      if (s.dockerUnreadableSince) counts.degraded++;
    } else if (s.connection === Connection.UNREACHABLE) counts.down++;
    else if (s.connection === Connection.PENDING) counts.waiting++;
  }
  return counts;
}

/** The agent versions the fleet runs, most servers first. Servers that haven't said are left out. */
export function agentVersions(servers: readonly Server[]): string[] {
  const seen = new Map<string, number>();
  for (const s of servers) {
    const v = s.agentBuild?.version;
    if (v) seen.set(v, (seen.get(v) ?? 0) + 1);
  }
  return [...seen.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).map(([v]) => v);
}
