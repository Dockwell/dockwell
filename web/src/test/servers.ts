// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  Connection,
  DiskUsageSchema,
  type Server,
  ServerInventorySchema,
  ServerSchema,
  type ServerStats,
  ServerStatsSchema,
  type WatchServersResponse,
  WatchServersResponseSchema,
} from "@dockwell/api";

const GiB = 1024 ** 3;
const HOUR = 3_600_000;

let clock = new Date(2026, 10, 4, 9, 30).getTime();

/**
 * One stats sample, 5 s after the one before: a share of CPU, memory in GiB
 * of an 8 GiB server, and uptime in hours. The rest is steady: 61 of 160 GiB
 * used on /, load 0.84, 0.91 and 0.88, and 1 MB/s in and 0.55 MB/s out.
 */
export function sample(cpu: number, usedGiB = 2.4, uptimeHours = 76, intervalMs = 5_000): ServerStats {
  clock += intervalMs;
  return create(ServerStatsSchema, {
    time: timestampFromDate(new Date(clock)),
    interval: durationFromMs(intervalMs),
    cpuPercent: cpu,
    memoryUsedBytes: BigInt(Math.round(usedGiB * GiB)),
    memoryTotalBytes: BigInt(8 * GiB),
    disks: [create(DiskUsageSchema, { mountPoint: "/", usedBytes: BigInt(61 * GiB), sizeBytes: BigInt(160 * GiB) })],
    load1: 0.84,
    load5: 0.91,
    load15: 0.88,
    networkRxBytesPerSecond: 1_000_000n,
    networkTxBytesPerSecond: 550_000n,
    uptime: durationFromMs(uptimeHours * HOUR),
  });
}

/** Moves the samples' clock on by ms, as if the agent stopped reporting for that long. */
export function gap(ms: number): void {
  clock += ms;
}

/** Samples 5 s apart with these CPU shares, for a server's history. */
export function history(...cpu: number[]): ServerStats[] {
  return cpu.map((c) => sample(c));
}

/** A connected server running Ubuntu, with the given fields on top. */
export function aServer(init: MessageInitShape<typeof ServerSchema>): Server {
  return create(ServerSchema, {
    connection: Connection.CONNECTED,
    inventory: create(ServerInventorySchema, { osName: "Ubuntu 24.04.1 LTS" }),
    ...init,
  });
}

/** A watch message carrying server. */
export function changed(server: Server): WatchServersResponse {
  return create(WatchServersResponseSchema, { server });
}

/** A watch message removing server. */
export function removed(server: Server): WatchServersResponse {
  return create(WatchServersResponseSchema, { server, removed: true });
}

/** The watch message that ends the snapshot. */
export const snapshotComplete: WatchServersResponse = create(WatchServersResponseSchema, { snapshotComplete: true });

/** What the control plane sends every 10 s while nothing changes, so a watch knows it's alive. */
export const heartbeat: WatchServersResponse = create(WatchServersResponseSchema, { heartbeat: true });
