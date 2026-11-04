// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  Connection,
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
 * of an 8 GiB server, and uptime in hours.
 */
export function sample(cpu: number, usedGiB = 2.4, uptimeHours = 76): ServerStats {
  clock += 5_000;
  return create(ServerStatsSchema, {
    time: timestampFromDate(new Date(clock)),
    cpuPercent: cpu,
    memoryUsedBytes: BigInt(Math.round(usedGiB * GiB)),
    memoryTotalBytes: BigInt(8 * GiB),
    uptime: durationFromMs(uptimeHours * HOUR),
  });
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
