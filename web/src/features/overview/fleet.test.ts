// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { BuildInfoSchema, Connection, ServerInventorySchema, type ServerStats, ServerStatsSchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { aServer } from "../../test/servers";
import { agentVersions, fleetCounts, fleetNumbers } from "./fleet";

const GiB = 1024 ** 3;
const T0 = Date.UTC(2026, 10, 8, 9, 0);

/** A sample s seconds after T0. */
function at(s: number, cpu: number, usedGiB: number, totalGiB: number, intervalMs?: number): ServerStats {
  return create(ServerStatsSchema, {
    time: timestampFromMs(T0 + s * 1000),
    interval: intervalMs === undefined ? undefined : durationFromMs(intervalMs),
    cpuPercent: cpu,
    memoryUsedBytes: BigInt(usedGiB * GiB),
    memoryTotalBytes: BigInt(totalGiB * GiB),
  });
}

function server(id: string, cpus: number, samples: ServerStats[], init = {}) {
  return aServer({
    id,
    name: id,
    inventory: create(ServerInventorySchema, { cpuCount: cpus }),
    stats: samples.at(-1),
    history: samples,
    ...init,
  });
}

describe("the fleet's numbers", () => {
  it("weighs each server's CPU by its cores, and adds up memory", () => {
    const big = server("big", 12, [at(0, 10, 4, 32)]);
    const small = server("small", 4, [at(1, 90, 3, 8)]);
    const numbers = fleetNumbers([big, small]);
    // (10 × 12 + 90 × 4) / 16
    expect(numbers?.now.cpuPercent).toBe(30);
    expect(numbers?.now.memoryUsedBytes).toBe(7 * GiB);
    expect(numbers?.now.memoryTotalBytes).toBe(40 * GiB);
    expect(numbers?.cpuCount).toBe(16);
  });

  it("leaves out a server that isn't reporting, and has none when no server is", () => {
    const up = server("up", 4, [at(0, 20, 2, 8)]);
    const down = server("down", 4, [at(0, 80, 6, 8)], { connection: Connection.UNREACHABLE });
    expect(fleetNumbers([up, down])?.now.cpuPercent).toBe(20);
    expect(fleetNumbers([down])).toBeUndefined();
  });

  it("draws the trend from when every server reporting began, each at its newest sample", () => {
    // One reports from 0 s, the other joins at 10 s, a second off the first's beat.
    const a = server("a", 2, [at(0, 10, 1, 4), at(5, 20, 1, 4), at(10, 30, 1, 4), at(15, 40, 1, 4)]);
    const b = server("b", 2, [at(11, 50, 2, 4), at(16, 70, 2, 4)]);
    const numbers = fleetNumbers([a, b]);
    expect(numbers?.trend.map((p) => p.cpuPercent)).toEqual([40, 55]);
    expect(numbers?.trend.map((p) => p.memoryUsedBytes)).toEqual([3 * GiB, 3 * GiB]);
    expect(numbers?.spanMs).toBe(10_000);
  });

  it("starts a server's part of the trend after its last gap", () => {
    // A pause of a minute: what came before it isn't part of the trend.
    const a = server("a", 1, [at(0, 90, 1, 4), at(60, 10, 1, 4), at(65, 20, 1, 4)]);
    expect(fleetNumbers([a])?.trend.map((p) => p.cpuPercent)).toEqual([10, 20]);
  });

  it("aligns slow samples with fast ones by the time their rates cover", () => {
    const slow = server("slow", 3, [at(0, 10, 1, 8, 15_000), at(15, 70, 3, 8, 15_000), at(20, 30, 2, 8)]);
    const fast = server("fast", 1, [
      at(0, 20, 1, 4),
      at(5, 40, 1, 4),
      at(10, 60, 1, 4),
      at(15, 80, 1, 4),
      at(20, 100, 1, 4),
    ]);
    const numbers = fleetNumbers([slow, fast]);
    expect(numbers?.trend.map((p) => p.cpuPercent)).toEqual([12.5, 62.5, 67.5, 72.5, 47.5]);
    expect(numbers?.trend.map((p) => p.memoryUsedBytes)).toEqual([2, 4, 4, 4, 3].map((n) => n * GiB));
    expect(numbers?.spanMs).toBe(25_000);
    expect(numbers?.now.cpuPercent).toBe(47.5);
  });

  it("keeps a slow sample's covered intervals after a real reporting gap", () => {
    const slow = server("slow", 1, [at(0, 90, 1, 4, 15_000), at(60, 10, 1, 4, 15_000), at(65, 20, 1, 4)]);
    const fast = server("fast", 1, [
      at(45, 100, 1, 4),
      at(50, 20, 1, 4),
      at(55, 40, 1, 4),
      at(60, 60, 1, 4),
      at(65, 80, 1, 4),
    ]);
    const numbers = fleetNumbers([slow, fast]);
    expect(numbers?.trend.map((p) => p.cpuPercent)).toEqual([15, 25, 35, 50]);
    expect(numbers?.spanMs).toBe(20_000);
  });
});

describe("the fleet's counts and agents", () => {
  it("counts a server that can't read Docker as serving, and as degraded", () => {
    const counts = fleetCounts([
      aServer({ id: "a" }),
      aServer({ id: "b", dockerUnreadableSince: timestampFromMs(T0) }),
      aServer({ id: "c", connection: Connection.UNREACHABLE }),
      aServer({ id: "d", connection: Connection.PENDING }),
    ]);
    expect(counts).toEqual({ total: 4, serving: 2, degraded: 1, down: 1, waiting: 1 });
  });

  it("lists the agents' versions, the most common first", () => {
    const build = (version: string) => ({ agentBuild: create(BuildInfoSchema, { version }) });
    expect(
      agentVersions([aServer(build("0.2.0")), aServer(build("0.2.1")), aServer(build("0.2.1")), aServer({})]),
    ).toEqual(["0.2.1", "0.2.0"]);
  });
});
