// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  ContainerUsageSchema,
  type ServerStats,
  ServerStatsSchema,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, places, SERVER } from "../../test/apps";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, sample, snapshotComplete } from "../../test/servers";
import { mergeUsage, ownOnly } from "../servers/useFleet";

const MiB = 1024 * 1024;

/** A sample of the server's own, with what web and another app use. */
function full(cpu: number, webCpu: number, webMiB: number, limitMiB = 512): ServerStats {
  const s = sample(cpu);
  s.containers = [
    create(ContainerUsageSchema, {
      containerId: "a".repeat(64),
      serviceId: "svc_web",
      cpuPercent: webCpu,
      memoryBytes: BigInt(webMiB * MiB),
      memoryLimitBytes: BigInt(limitMiB * MiB),
    }),
    create(ContainerUsageSchema, { containerId: "b".repeat(64), serviceId: "svc_other", cpuPercent: 80 }),
  ];
  return s;
}

/** A sample as someone below an admin is sent it: its time, and their app alone. */
function theirs(webCpu: number, webMiB: number): ServerStats {
  const s = full(0, webCpu, webMiB);
  return create(ServerStatsSchema, { time: s.time, interval: s.interval, containers: [s.containers[0] ?? {}] });
}

function api(history: ServerStats[], resources: { cpus?: string; memory?: string } = {}): Api {
  const web = anApp("web", "nginx:1.27", { resources });
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1", history, stats: history.at(-1) })), snapshotComplete);
  return {
    resources: places(),
    services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: [] }) },
    servers: { watchServers: watch.stream },
    changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
  };
}

describe("an app's Metrics card (#797)", () => {
  it("says what it uses over the last 5 minutes, against its limit, with nothing of the server's or another app's", async () => {
    const { container } = renderAt("/apps/svc_web", api([full(40, 10, 100), full(40, 25, 200)], { cpus: "0.5" }));
    const metrics = await screen.findByRole("region", { name: "Metrics" });
    expect(metrics).toHaveTextContent("Last 5 minutes");
    // 25% of a CPU is half its limit of 0.5 vCPU.
    expect(metrics).toHaveTextContent("CPU50% of its limit");
    expect(metrics).toHaveTextContent("Memory200 MBof 512 MB");
    expect(metrics).not.toHaveTextContent("80");
    await expectAccessible(container);
  });

  it("says its CPU as a share of one CPU where it has no limit", async () => {
    renderAt("/apps/svc_web", api([full(40, 3.24, 64, 0)]));
    const metrics = await screen.findByRole("region", { name: "Metrics" });
    expect(metrics).toHaveTextContent("CPU3.2% of a CPU");
    expect(metrics).not.toHaveTextContent("of 512");
  });

  it("reads the same from what someone below an admin is sent, its server's own figures left out", async () => {
    renderAt("/apps/svc_web", api([theirs(10, 100), theirs(25, 200)], { cpus: "0.5" }));
    const metrics = await screen.findByRole("region", { name: "Metrics" });
    expect(metrics).toHaveTextContent("CPU50% of its limit");
  });

  it("isn't there until its servers' samples say what it uses", async () => {
    renderAt("/apps/svc_web", api([sample(40)]));
    await screen.findByRole("region", { name: "Configuration" });
    expect(screen.queryByRole("region", { name: "Metrics" })).toBeNull();
  });

  it("says what it uses beside its limits in Settings", async () => {
    renderAt("/apps/svc_web/settings", api([full(40, 25, 200)], { cpus: "0.5", memory: "512M" }));
    const resources = await screen.findByRole("region", { name: "Resources" });
    const cpu = await within(resources).findByRole("combobox", { name: "CPU limit" });
    await waitFor(() => expect(cpu).toHaveAccessibleDescription(/^Uses 25% of a CPU/));
    // The test's server says nothing of its memory, so the line is its use alone.
    expect(within(resources).getByRole("combobox", { name: "Memory limit" })).toHaveAccessibleDescription(
      "Uses 200 MB",
    );
  });
});

describe("a server's samples that hold only apps' figures (#797)", () => {
  it("stay out of the server's own figures, and go to its apps'", () => {
    const own = full(40, 25, 200);
    const apps = theirs(25, 200);
    const server = aServer({ id: SERVER, name: "hz-1", history: [apps], stats: apps });
    const kept = ownOnly(server);
    expect(kept.stats).toBeUndefined();
    expect(kept.history).toEqual([]);
    expect(mergeUsage(undefined, server)).toEqual([apps]);
    // A server's own samples are its own, as before.
    const admin = aServer({ id: SERVER, name: "hz-1", history: [own], stats: own });
    expect(ownOnly(admin)).toBe(admin);
    // A later message appends its newest sample, once.
    const next = theirs(30, 210);
    const after = mergeUsage([apps], aServer({ id: SERVER, name: "hz-1", stats: next }));
    expect(after).toHaveLength(2);
    expect(mergeUsage(after, aServer({ id: SERVER, name: "hz-1", stats: next }))).toHaveLength(2);
  });
});
