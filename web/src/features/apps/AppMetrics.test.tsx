// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  Connection,
  ContainerUsageSchema,
  DeploymentState,
  type ServerStats,
  ServerStatsSchema,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, places, SERVER } from "../../test/apps";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, sample, snapshotComplete } from "../../test/servers";
import { mergeUsage, ownOnly } from "../servers/useFleet";

const MiB = 1024 * 1024;

/** A sample of the server's own, with what web's replicas and another app use, each service's folded into one. */
function full(cpu: number, webCpu: number, webMiB: number, limitMiB = 512, replicas = 1): ServerStats {
  const s = sample(cpu);
  s.containers = [
    create(ContainerUsageSchema, {
      serviceId: "svc_web",
      cpuPercent: webCpu,
      memoryBytes: BigInt(webMiB * MiB),
      memoryLimitBytes: BigInt(limitMiB * MiB * replicas),
      replicas,
    }),
    create(ContainerUsageSchema, { serviceId: "svc_other", cpuPercent: 80, replicas: 1 }),
  ];
  return s;
}

/** A sample as someone below an admin is sent it: its time, and their app alone, or none of it. */
function theirs(webCpu: number, webMiB: number, running = true): ServerStats {
  const s = full(0, webCpu, webMiB);
  return create(ServerStatsSchema, {
    time: s.time,
    interval: s.interval,
    containers: running ? [s.containers[0] ?? {}] : [],
  });
}

function api(
  history: ServerStats[],
  resources: { cpus?: string; memory?: string } = {},
  connection = Connection.CONNECTED,
): Api {
  const web = anApp("web", "nginx:1.27", { resources });
  const watch = feed<WatchServersResponse>();
  watch.push(
    changed(aServer({ id: SERVER, name: "hz-1", connection, history, stats: history.at(-1) })),
    snapshotComplete,
  );
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
    expect(metrics).toHaveTextContent("Last 5 minutes, all replicas");
    // 25% of a CPU is half its limit of 0.5 vCPU.
    expect(metrics).toHaveTextContent("CPU50% of limit");
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
    expect(metrics).toHaveTextContent("CPU50% of limit");
  });

  it("compares every replica with every replica's limit: 3 at half a CPU under a limit of 1 read 50% (#848's review, M2)", async () => {
    renderAt("/apps/svc_web", api([full(40, 150, 300, 256, 3)], { cpus: "1" }));
    const metrics = await screen.findByRole("region", { name: "Metrics" });
    expect(metrics).toHaveTextContent("CPU50% of limit");
    expect(metrics).toHaveTextContent("Memory300 MBof 768 MB");
  });

  it("goes once its app stops, rather than show its last figures as now (#848's review, M1)", async () => {
    renderAt("/apps/svc_web", api([theirs(10, 100), theirs(25, 200), theirs(0, 0, false)], { cpus: "0.5" }));
    await screen.findByRole("region", { name: "Configuration" });
    expect(screen.queryByRole("region", { name: "Metrics" })).toBeNull();
  });

  it("goes once its server is unreachable, rather than show its last sample as now (#852)", async () => {
    renderAt("/apps/svc_web", api([theirs(10, 100), theirs(25, 200)], { cpus: "0.5" }, Connection.UNREACHABLE));
    await screen.findByRole("region", { name: "Configuration" });
    expect(screen.queryByRole("region", { name: "Metrics" })).toBeNull();
  });

  it("leaves an unreachable server's use out of Settings' Resources (#852)", async () => {
    renderAt("/apps/svc_web/settings", api([full(40, 25, 200)], { cpus: "0.5" }, Connection.UNREACHABLE));
    const resources = await screen.findByRole("region", { name: "Resources" });
    const cpu = await within(resources).findByRole("combobox", { name: "CPU limit" });
    expect(cpu).not.toHaveAccessibleDescription(/Uses/);
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

describe("an app's Metrics card with a server not reporting, or the fleet not heard (#852's review)", () => {
  /** web, live on hz-1 and hz-2, with hz-2 as connection says, and the watch, to fail. */
  function twoServers(connection: Connection) {
    const at = timestampFromDate(new Date(Date.now() - 3_600_000));
    const web = anApp(
      "web",
      "nginx:1.27",
      { resources: { cpus: "0.5" } },
      {
        deployments: [
          { serverId: SERVER, serverName: "hz-1", state: DeploymentState.LIVE, updateTime: at },
          { serverId: "srv_2", serverName: "hz-2", state: DeploymentState.LIVE, updateTime: at },
        ],
      },
    );
    const watch = feed<WatchServersResponse>();
    const history = [theirs(10, 100), theirs(25, 200)];
    watch.push(
      changed(aServer({ id: SERVER, name: "hz-1", history, stats: history.at(-1) })),
      changed(aServer({ id: "srv_2", name: "hz-2", connection, history, stats: history.at(-1) })),
      snapshotComplete,
    );
    const api: Api = {
      resources: places(),
      services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: [] }) },
      servers: { watchServers: watch.stream },
      changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    };
    return { api, watch };
  }

  it("says a server isn't reporting, rather than all replicas, and counts the rest alone (L3)", async () => {
    renderAt("/apps/svc_web", twoServers(Connection.UNREACHABLE).api);
    const metrics = await screen.findByRole("region", { name: "Metrics" });
    expect(metrics).toHaveTextContent("1 server not reporting");
    expect(metrics).not.toHaveTextContent("all replicas");
    // hz-1's 200 MB alone, not hz-2's last 200 MB beside it.
    expect(metrics).toHaveTextContent("Memory200 MB");
  });

  it("says all replicas while every server reports", async () => {
    renderAt("/apps/svc_web", twoServers(Connection.CONNECTED).api);
    const metrics = await screen.findByRole("region", { name: "Metrics" });
    expect(metrics).toHaveTextContent("Last 5 minutes, all replicas");
    // Both servers' 200 MB together.
    expect(metrics).toHaveTextContent("Memory400 MB");
  });

  it("reads as not current while the panel's watch of the fleet reconnects, as the overview's figures do (L4)", async () => {
    const { api, watch } = twoServers(Connection.CONNECTED);
    const { container } = renderAt("/apps/svc_web", api);
    await screen.findByRole("region", { name: "Metrics" });
    expect(container.querySelector(".app-metrics")).not.toHaveClass("is-stale");
    act(() => watch.fail(new ConnectError("the watch fell behind", Code.Unavailable)));
    await waitFor(() => expect(container.querySelector(".app-metrics")).toHaveClass("is-stale"));
    expect(container.querySelector(".app-metrics")).toHaveAttribute("aria-disabled", "true");
  });

  it("leaves what it used out of Settings' hints while the panel's watch reconnects (L4)", async () => {
    const { api, watch } = twoServers(Connection.CONNECTED);
    renderAt("/apps/svc_web/settings", api);
    const resources = await screen.findByRole("region", { name: "Resources" });
    const cpu = await within(resources).findByRole("combobox", { name: "CPU limit" });
    await waitFor(() => expect(cpu).toHaveAccessibleDescription(/^Uses/));
    act(() => watch.fail(new ConnectError("the watch fell behind", Code.Unavailable)));
    await waitFor(() => expect(cpu).not.toHaveAccessibleDescription(/Uses/));
  });
});

describe("Settings' Resources with several replicas (#848's re-check, M-A)", () => {
  it("says each replica's use beside a limit that's each replica's", async () => {
    // 3 replicas, each at half a CPU and 200 MB, under 1 CPU and 256M each.
    renderAt("/apps/svc_web/settings", api([full(40, 150, 600, 256, 3)], { cpus: "1", memory: "256M" }));
    const resources = await screen.findByRole("region", { name: "Resources" });
    const cpu = await within(resources).findByRole("combobox", { name: "CPU limit" });
    await waitFor(() => expect(cpu).toHaveAccessibleDescription(/^Uses 50% of a CPU each/));
    expect(within(resources).getByRole("combobox", { name: "Memory limit" })).toHaveAccessibleDescription(
      "Uses 200 MB each",
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
    // A viewer's sample with none of their apps in it stays off the server's own too (M1).
    expect(ownOnly(aServer({ id: SERVER, name: "hz-1", stats: theirs(0, 0, false) })).stats).toBeUndefined();
    // An admin's sample from an agent that couldn't read the server's memory is the server's (L5).
    const noMemory = full(40, 25, 200);
    noMemory.memoryTotalBytes = 0n;
    expect(ownOnly(aServer({ id: SERVER, name: "hz-1", stats: noMemory })).stats).toBe(noMemory);
    // A sample of disks alone, with no memory or uptime, is the server's too (#849's websites test).
    const disksOnly = create(ServerStatsSchema, { disks: [{ mountPoint: "/", sizeBytes: 100n, usedBytes: 40n }] });
    expect(ownOnly(aServer({ id: SERVER, name: "hz-1", stats: disksOnly })).stats).toBe(disksOnly);
    // A later message appends its newest sample, once.
    const next = theirs(30, 210);
    const after = mergeUsage([apps], aServer({ id: SERVER, name: "hz-1", stats: next }));
    expect(after).toHaveLength(2);
    expect(mergeUsage(after, aServer({ id: SERVER, name: "hz-1", stats: next }))).toHaveLength(2);
  });
});
