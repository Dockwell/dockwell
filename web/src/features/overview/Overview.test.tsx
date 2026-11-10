// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import {
  BackupCopy,
  type BackupPolicyInfo,
  BackupPolicyInfoSchema,
  BackupSchema,
  BackupState,
  BuildInfoSchema,
  Connection,
  DeploymentState,
  EventKind,
  HealthState,
  PackageManager,
  type Server,
  ServerInventorySchema,
  ServerStrain,
  type WatchEventsResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { aHealth, anApp, ENV, places, running } from "../../test/apps";
import { applied, changeEvent, since, snapshot, webPlanned } from "../../test/changes";
import { expectMetric, metric } from "../../test/metrics";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, gap, history, sample, snapshotComplete } from "../../test/servers";

const GiB = 1024 ** 3;

const inventory = create(ServerInventorySchema, {
  hostname: "hz-1",
  osName: "Ubuntu 24.04.1 LTS",
  cpuCount: 4,
  memoryBytes: BigInt(8 * GiB),
  addresses: ["49.12.33.7"],
});
const past = history(20, 22, 23.4);
const hz1 = aServer({ id: "srv_1", name: "hz-1", controlServer: true, inventory, stats: past.at(-1), history: past });

/** Renders the overview with a watch the test feeds. */
function renderOverview(watch = feed<WatchServersResponse>()) {
  const rendered = renderAt("/", { servers: { watchServers: watch.stream } });
  return { ...rendered, watch };
}

afterEach(() => {
  vi.useRealTimers();
});

describe("Overview with one server", () => {
  it("shows the server's health, live", async () => {
    const { watch } = renderOverview();
    watch.push(changed(hz1), snapshotComplete);

    expect(await screen.findByText("4 vCPU · 8 GB · Ubuntu 24.04.1 LTS · up 3 days")).toBeVisible();
    expect(screen.getByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
    // Three samples, 5 s apart, cover 15 s.
    expectMetric("CPU", "23.4", "%", "22% on average over the last 15 s");
    expectMetric("Memory", "2.4", "of 8 GB", "30% in use");
    expectMetric("Disk", "61", "of 160 GB", "99 GB free");
    expectMetric("Network", "12", "Mbit/s", "8 Mbit/s in · 4.4 Mbit/s out");
    for (const name of ["CPU", "Memory", "Disk", "Network"]) {
      expect(screen.getByRole("img", { name: `${name} trend` })).toBeVisible();
    }
    // Healthy servers stay quiet.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("updates the numbers in place as samples arrive", async () => {
    const { watch } = renderOverview();
    watch.push(changed(hz1), snapshotComplete);
    const value = await within(await waitFor(() => metric("CPU"))).findByText("23.4");

    act(() => watch.push(changed(aServer({ ...hz1, history: [], stats: sample(57) }))));

    await waitFor(() => expect(value).toHaveTextContent("57.0"));
    expect(value).toBeInTheDocument();
    expect(metric("CPU")).toHaveTextContent("last 20 s");
  });

  it("captions a full history as the 5 min the control plane keeps", async () => {
    const { watch } = renderOverview();
    const full = history(...Array.from({ length: 60 }, (_, i) => 20 + (i % 5)));
    watch.push(changed(aServer({ ...hz1, stats: full.at(-1), history: full })), snapshotComplete);

    await waitFor(() => expectMetric("CPU", "%", "on average over the last 5 min"));
    expect(screen.getByRole("img", { name: "CPU trend" })).toBeVisible();
  });

  it("draws the trend from the last gap in reporting, not across it", async () => {
    const { watch } = renderOverview();
    const before = history(80, 85, 90, 95);
    gap(2 * 60_000);
    const after = history(10, 11, 12);
    const all = [...before, ...after];
    watch.push(changed(aServer({ ...hz1, stats: all.at(-1), history: all })), snapshotComplete);

    await waitFor(() => expectMetric("CPU", "12.0", "%", "11% on average over the last 15 s"));
    // One point per sample since the gap.
    expect(metric("CPU").querySelector(".dw-spark-line")?.getAttribute("d")?.match(/[ML]/g)).toHaveLength(after.length);
  });

  it("says when the server stops reporting, and how to fix it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: new Date(2026, 10, 4, 12, 0) });
    const { watch } = renderOverview();
    const seen = timestampFromDate(new Date(2026, 10, 4, 11, 57));
    watch.push(changed(aServer({ ...hz1, connection: Connection.UNREACHABLE, lastSeenTime: seen })), snapshotComplete);

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("hz-1 isn't reporting");
    expect(alert).toHaveTextContent(
      "Its agent last reported 3 min ago, and the numbers here are from then. Check that dockwell run is still running on the server.",
    );
    expect(within(alert).getByText("dockwell run").tagName).toBe("CODE");
    expect(metric("CPU")).toHaveTextContent("23.4");
    // Uptime was true at the last report, not now.
    expect(screen.getByText("4 vCPU · 8 GB · Ubuntu 24.04.1 LTS")).toBeVisible();
  });

  it("counts the time since the last report second by second", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: new Date(2026, 10, 4, 12, 0) });
    const { watch } = renderOverview();
    const seen = timestampFromDate(new Date(2026, 10, 4, 11, 59, 50));
    watch.push(changed(aServer({ ...hz1, connection: Connection.UNREACHABLE, lastSeenTime: seen })), snapshotComplete);

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("last reported 10 s ago");
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    expect(alert).toHaveTextContent("last reported 11 s ago");
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    expect(alert).toHaveTextContent("last reported 12 s ago");
  });

  it("explains a server whose agent hasn't connected yet", async () => {
    const { watch } = renderOverview();
    watch.push(
      changed(aServer({ id: "srv_1", name: "hz-1", connection: Connection.PENDING, inventory: undefined })),
      snapshotComplete,
    );

    expect(await screen.findByText("hz-1 is waiting for its agent")).toBeVisible();
    expect(screen.getByRole("status")).toHaveTextContent("Start it with dockwell run on the server.");
    expect(screen.queryByText("CPU")).toBeNull();
  });

  it("keeps the numbers and says so while it reconnects", async () => {
    const { watch } = renderOverview();
    watch.push(changed(hz1), snapshotComplete);
    await screen.findByText("23.4");

    act(() => watch.fail(new ConnectError("the watch fell behind", Code.Unavailable)));

    // On wide screens the top bar says so, for the page too.
    const note = await within(screen.getByRole("banner")).findByRole("status");
    expect(note).toHaveTextContent("Reconnecting");
    expect(screen.getAllByRole("status")).toEqual([note]);
    expect(screen.getByText("23.4")).toBeVisible();
  });

  it("says so in the page's head on phones, which have no top bar", async () => {
    onPhone();
    const { watch } = renderOverview();
    watch.push(changed(hz1), snapshotComplete);
    await screen.findByText("23.4");

    act(() => watch.fail(new ConnectError("the watch fell behind", Code.Unavailable)));

    const note = await within(screen.getByRole("main")).findByRole("status");
    expect(note).toHaveTextContent("Reconnecting");
    expect(screen.getAllByRole("status")).toEqual([note]);
  });

  it("puts the server in the phone header and the numbers two by two", async () => {
    onPhone();
    const { watch, container } = renderOverview();
    watch.push(changed(hz1), snapshotComplete);

    expect(await screen.findByRole("heading", { level: 1, name: "hz-1" })).toBeVisible();
    expect(screen.getByText("Healthy · up 3 days")).toBeVisible();
    expectMetric("CPU", "23.4", "%");
    expect(screen.queryByRole("img", { name: "CPU trend" })).toBeNull();
    expect(screen.queryByText("22% on average over the last 15 s")).toBeNull();
    await expectAccessible(container);
  });

  it("passes axe", async () => {
    const { watch, container } = renderOverview();
    watch.push(changed(aServer({ ...hz1, connection: Connection.UNREACHABLE })), snapshotComplete);
    await screen.findByRole("alert");
    await expectAccessible(container);
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    const { watch } = renderOverview();
    watch.push(changed(hz1), snapshotComplete);

    expect(await screen.findByText("4 个 vCPU · 8 GB · Ubuntu 24.04.1 LTS · 已运行 3 天")).toBeVisible();
    expect(screen.getByRole("heading", { level: 1, name: "概览" })).toBeVisible();
    expectMetric("内存", "2.4", "/ 8 GB", "已使用 30%");
    expectMetric("磁盘", "61", "/ 160 GB", "可用 99 GB");
    expectMetric("网络", "接收 8 Mbit/s · 发送 4.4 Mbit/s");
  });
});

describe("Overview with no servers", () => {
  it("says how to add one", async () => {
    const { watch, container } = renderOverview();
    watch.push(snapshotComplete);

    expect(await screen.findByRole("heading", { level: 2, name: "No servers yet" })).toBeVisible();
    const text = screen.getByText(/on the control server/);
    expect(text).toHaveTextContent(
      "Run dockwell token create on the control server, then dockwell join with the ticket it prints on the server you want to add.",
    );
    expect(within(text).getByText("dockwell token create").tagName).toBe("CODE");
    await expectAccessible(container);
  });
});

describe("Overview while it connects", () => {
  it("says it is loading only when the servers are slow", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderOverview();
    await screen.findByRole("heading", { level: 1, name: "Overview" });
    expect(screen.getByText("Loading servers")).toHaveClass("is-quiet");
    await act(() => vi.advanceTimersByTimeAsync(350));
    expect(screen.getByText("Loading servers")).not.toHaveClass("is-quiet");
  });

  it("explains servers that didn't load", async () => {
    const watch = feed<WatchServersResponse>();
    watch.fail(new ConnectError("sign in to see servers", Code.Unauthenticated));
    renderOverview(watch);

    expect(await screen.findByText("The server list didn't load. Dockwell keeps trying.")).toBeVisible();
    expect(screen.getByText("sign in to see servers")).toBeVisible();
  });
});

describe("Overview with a fleet", () => {
  const big = create(ServerInventorySchema, {
    osName: "Debian GNU/Linux 13",
    cpuCount: 12,
    addresses: ["198.51.100.20"],
  });
  const agent = (version: string) => create(BuildInfoSchema, { version });
  const hz2 = (init: Parameters<typeof aServer>[0] = {}) =>
    aServer({ id: "srv_2", name: "hz-2", inventory: big, stats: sample(70, 6), agentBuild: agent("0.2.1"), ...init });
  const fleet = () => [aServer({ ...hz1, agentBuild: agent("0.2.1") }), hz2()];

  /** The overview of these servers, with the apps, health and events the test gives. */
  function renderFleet(servers: Server[], extra: Partial<Api> = {}) {
    const watch = feed<WatchServersResponse>();
    watch.push(...servers.map(changed), snapshotComplete);
    const rendered = renderAt("/", { servers: { watchServers: watch.stream }, ...extra });
    return { ...rendered, watch };
  }

  it("leads with how many servers serve and the fleet's numbers, each server a tile that opens it", async () => {
    const user = userEvent.setup();
    const { router } = renderFleet(fleet());

    expect(await screen.findByText("2 servers · agent 0.2.1 everywhere", { selector: ".page-lead" })).toBeVisible();
    expectMetric("Servers serving", "2", "of 2", "Every server is reporting");
    // Each server's CPU counts for its cores: (23.4 × 4 + 70 × 12) / 16.
    expectMetric("CPU", "58.4", "%", "Across 16 vCPU");
    // 2.4 and 6 GiB of 8 each.
    expectMetric("Memory", "53", "%", "8.4 of 16 GB");
    expect(screen.queryByText("Requests")).toBeNull();

    const map = screen.getByRole("region", { name: "Servers" });
    expect(within(map).getByRole("link", { name: "View all" })).toHaveAttribute("href", "/servers");
    await user.hover(within(map).getByRole("button", { name: "hz-2, Healthy, CPU 70%" }));
    expect(within(map).getByText("198.51.100.20")).toBeVisible();
    await user.click(within(map).getByRole("button", { name: "hz-2, Healthy, CPU 70%" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/servers/srv_2"));
  });

  it.each([
    ["v0.2.2-0.20261108005608-b92efc90d686", "2 servers · agent 0.2.2-dev everywhere"],
    ["0.2.0", "2 servers · agents on 2 versions"],
  ])("says which agents run, a build on the way to a release as the rail says it (%s)", async (other, lead) => {
    const wip = agent("v0.2.2-0.20261108005608-b92efc90d686");
    renderFleet([aServer({ ...hz1, agentBuild: wip }), hz2({ agentBuild: agent(other) })]);
    expect(await screen.findByText(lead, { selector: ".page-lead" })).toBeVisible();
  });

  it("says first which server stopped reporting, and leaves its numbers out of the fleet's", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(new Date(2026, 10, 8, 9, 30));
    const lastSeenTime = timestampFromDate(new Date(2026, 10, 8, 9, 27));
    renderFleet([hz1, hz2({ connection: Connection.UNREACHABLE, lastSeenTime })]);

    const callout = await screen.findByRole("alert");
    expect(callout).toHaveTextContent("hz-2 isn't reporting");
    expect(callout).toHaveTextContent(
      "Its agent last reported 3 min ago, so its numbers are left out of the fleet's. Check that dockwell run is still running on the server.",
    );
    expect(within(callout).getByRole("link", { name: "Inspect" })).toHaveAttribute("href", "/servers/srv_2");
    expectMetric("Servers serving", "1", "of 2", "1 down");
    expectMetric("CPU", "23.4", "%", "Across 4 vCPU");
    expect(screen.getByRole("button", { name: "hz-2, Down" })).toBeVisible();
  });

  it("names several servers that stopped reporting in one callout, and opens Servers", async () => {
    renderFleet([
      hz1,
      hz2({ connection: Connection.UNREACHABLE }),
      aServer({ id: "srv_3", name: "hz-3", connection: Connection.UNREACHABLE }),
    ]);

    const callout = await screen.findByRole("alert");
    expect(callout).toHaveTextContent("2 servers aren't reporting");
    expect(callout).toHaveTextContent("hz-2 and hz-3 stopped reporting");
    expect(within(callout).getByRole("link", { name: "Inspect" })).toHaveAttribute("href", "/servers");
  });

  it("says when a server can't read Docker, and counts it as serving", async () => {
    renderFleet([hz1, hz2({ dockerUnreadableSince: timestampFromDate(new Date()) })]);

    expect(await screen.findByText("hz-2 can't read Docker")).toBeVisible();
    expectMetric("Servers serving", "2", "of 2", "1 can't read Docker");
  });

  it("says why a strained server reads Degraded, in the server list's words", async () => {
    const user = userEvent.setup();
    renderFleet([hz1, hz2({ strainedSince: timestampFromDate(new Date()), strain: ServerStrain.MEMORY })]);

    const map = await screen.findByRole("region", { name: "Servers" });
    await user.hover(within(map).getByRole("button", { name: /^hz-2, Degraded/ }));
    expect(within(map).getByText("Memory above 90%")).toBeVisible();
  });

  it("lists the apps with where each runs, and what changed, apps' health included", async () => {
    const kinds: EventKind[][] = [];
    const web = anApp(
      "web",
      "ghcr.io/example/web:1.5.0",
      {},
      {
        deployments: [
          { serverId: "srv_2", serverName: "hz-2", state: DeploymentState.LIVE },
          { serverId: "srv_1", serverName: "hz-1", state: DeploymentState.LIVE },
        ],
      },
    );
    renderFleet(fleet(), {
      services: { listServices: () => ({ services: [web] }) },
      resources: places(),
      health: running(web),
      domains: { listDomains: () => ({}) },
      events: {
        watchEvents: eventsWatch(changeEvent(applied(webPlanned()))),
        listEvents: (req: { kinds: EventKind[] }) => {
          kinds.push(req.kinds);
          return { events: [changeEvent(applied(webPlanned()))] };
        },
      },
    });

    const table = await screen.findByRole("table", { name: "Apps on your servers" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(2));
    expect(within(table).getAllByRole("row")[1]).toHaveTextContent("2 servershz-1, hz-2");
    expect(within(table).queryByRole("columnheader", { name: "Deployed" })).toBeNull();
    const changes = screen.getByRole("region", { name: "Recent changes" });
    expect(await within(changes).findByText("Update web")).toBeVisible();
    expect(kinds).toContainEqual([EventKind.CHANGE, EventKind.HEALTH]);
  });

  it("calls itself Fleet on a phone, with CPU and memory two by two and the map without its readout", async () => {
    onPhone();
    renderFleet(fleet());

    expect(await screen.findByRole("heading", { level: 1, name: "Fleet" })).toBeVisible();
    expectMetric("CPU", "58.4", "%");
    expectMetric("Memory", "8.4", "of 16 GB");
    expect(screen.queryByText("Servers serving")).toBeNull();
    expect(screen.getByRole("button", { name: "hz-2, Healthy, CPU 70%" })).toBeVisible();
    expect(screen.queryByText("Hover or focus a server to inspect it.")).toBeNull();
    // What changed sits under its own heading, with no panel round it.
    const changes = screen.getByRole("region", { name: "Recent changes" });
    expect(changes).not.toHaveClass("dw-panel");
    expect(within(changes).getByRole("heading", { level: 2, name: "Recent changes" })).toBeVisible();
  });

  it("reads every server as Unknown once what the fleet last said can't stand", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let answering = true;
    renderAt("/", {
      servers: {
        async *watchServers(_req: unknown, ctx: HandlerContext) {
          if (answering) {
            yield changed(hz1);
            yield changed(hz2());
            yield snapshotComplete;
          }
          await new Promise<void>((resolve) => ctx.signal.addEventListener("abort", () => resolve(), { once: true }));
        },
      },
    });
    await screen.findByRole("button", { name: "hz-2, Healthy, CPU 70%" });
    answering = false;
    await vi.advanceTimersByTimeAsync(35_000);
    expect(await screen.findByRole("button", { name: "hz-2, Unknown" })).toBeVisible();
    expect(screen.queryByRole("button", { name: /Healthy/ })).toBeNull();
  });

  it("passes axe", async () => {
    const { container } = renderFleet([hz1, hz2({ connection: Connection.UNREACHABLE })]);
    await screen.findByRole("alert");
    await expectAccessible(container);
  });

  it.each([
    ["wide", "banner"],
    ["phone", "main"],
  ] as const)("says once that it is reconnecting, %s", async (layout, where) => {
    if (layout === "phone") onPhone();
    const { watch } = renderOverview();
    watch.push(changed(hz1), changed(aServer({ id: "srv_2", name: "hz-2", stats: sample(8) })), snapshotComplete);
    await screen.findByRole("button", { name: /^hz-2, / });

    act(() => watch.fail(new ConnectError("the watch fell behind", Code.Unavailable)));

    const note = await within(screen.getByRole(where)).findByRole("status");
    expect(note).toHaveTextContent("Reconnecting");
    expect(screen.getAllByText("Reconnecting")).toEqual([note]);
    expect(screen.getAllByRole("status")).toEqual([note]);
  });
});

describe("the server list page", () => {
  it("says how many servers there are under its title", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), changed(aServer({ id: "srv_2", name: "hz-2", stats: sample(8) })), snapshotComplete);
    renderAt("/servers", { servers: { watchServers: watch.stream } });
    await screen.findByRole("button", { name: "hz-2, Healthy, CPU 8%" });
    expect(screen.getByText("2 servers", { selector: ".page-lead" })).toBeVisible();
  });

  it.each([
    ["wide", "banner"],
    ["phone", "main"],
  ] as const)("says once that it is reconnecting, %s", async (layout, where) => {
    if (layout === "phone") onPhone();
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), changed(aServer({ id: "srv_2", name: "hz-2", stats: sample(8) })), snapshotComplete);
    renderAt("/servers", { servers: { watchServers: watch.stream } });
    await screen.findByRole("button", { name: /^hz-2, / });

    act(() => watch.fail(new ConnectError("the watch fell behind", Code.Unavailable)));

    const note = await within(screen.getByRole(where)).findByRole("status");
    expect(note).toHaveTextContent("Reconnecting");
    expect(screen.getAllByText("Reconnecting")).toEqual([note]);
    expect(screen.getAllByRole("status")).toEqual([note]);
  });
});

/** A watch that opens with these events and stays open. */
function eventsWatch(...events: Parameters<typeof snapshot>[0]) {
  return async function* (_req: unknown, ctx: HandlerContext): AsyncGenerator<WatchEventsResponse> {
    yield snapshot(events);
    await new Promise<void>((resolve) => ctx.signal.addEventListener("abort", () => resolve(), { once: true }));
  };
}

describe("what the overview shows below the numbers", () => {
  const web = anApp("web", "ghcr.io/example/web:1.5.0");
  const api = anApp("api", "ghcr.io/example/api:2.1.0");

  function overviewApi(backups: Api["backups"] = {}, volumes = false): Api {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete);
    return {
      servers: { watchServers: watch.stream },
      services: { listServices: () => ({ services: [web, api] }) },
      resources: {
        listResources: (req: { kind: string }) =>
          req.kind === "volume"
            ? {
                resources: volumes ? [{ id: "vol_1", resource: { kind: "volume", scope: ENV, name: "orders" } }] : [],
              }
            : places().listResources(req),
      },
      health: {
        listHealth: () => ({
          services: [
            aHealth(web.id, { state: HealthState.HEALTHY }),
            aHealth(api.id, { state: HealthState.UNHEALTHY, said: "the database refuses connections" }),
          ],
        }),
      },
      domains: {
        listDomains: () => ({
          domains: [
            { name: "www.example.com", environmentId: ENV, routes: [{ service: "web", port: 3000 }] },
            { name: "example.com", environmentId: ENV, routes: [{ service: "web", port: 3000 }] },
          ],
        }),
      },
      events: {
        watchEvents: eventsWatch(changeEvent(applied(webPlanned()))),
        // Recent changes reads its own newest page of changes alone.
        listEvents: () => ({ events: [changeEvent(applied(webPlanned()))] }),
      },
      backups: {
        listBackups: () => ({ backups: [] }),
        listBackupPolicies: () => ({ policies: [] }),
        listBackupDestinations: () => ({ destinations: [] }),
        ...backups,
      },
    };
  }

  it("lists the apps, what needs a look first, and where each answers", async () => {
    renderAt("/", overviewApi());

    const table = await screen.findByRole("table", { name: "Apps on this server" });
    await waitFor(() =>
      expect(
        within(table)
          .getAllByRole("row")
          .map((r) => r.textContent),
      ).toHaveLength(3),
    );
    const [, first, second] = within(table).getAllByRole("row");
    await waitFor(() => expect(first).toHaveTextContent(/^api/));
    expect(first).toHaveTextContent("Unhealthy");
    expect(second).toHaveTextContent("web");
    expect(second).toHaveTextContent("Healthy");
    await waitFor(() => expect(second).toHaveTextContent("www.example.com+1"));
    expect(screen.getByText("2 apps · 1 needs a look")).toBeVisible();
    expect(screen.getByRole("link", { name: "View all" })).toHaveAttribute("href", "/apps");
    expect(screen.getByRole("link", { name: "Files" })).toHaveAttribute("href", "/servers/srv_1/files");
  });

  it("shows the newest changes, and how backups stand", async () => {
    const { container } = renderAt("/", overviewApi());

    const changes = await screen.findByRole("region", { name: "Recent changes" });
    expect(await within(changes).findByText("Update web")).toBeVisible();
    expect(within(changes).getByRole("link", { name: "Activity" })).toHaveAttribute("href", "/activity");
    const backups = screen.getByRole("region", { name: "Backups" });
    expect(await within(backups).findByText("Nothing to back up yet: no app keeps data.")).toBeVisible();
    await expectAccessible(container);
  });

  const updates: Api["updates"] = {
    getServerUpdates: () => ({
      updates: {
        serverId: "srv_1",
        manager: PackageManager.APT,
        marksSecurity: true,
        readTime: timestampFromDate(new Date()),
        packages: [
          {
            name: "openssl",
            installed: "3.0.13-0ubuntu3.4",
            available: "3.0.13-0ubuntu3.5",
            origin: "noble-security",
            security: true,
          },
        ],
      },
    }),
  };

  it("puts the updates last beside them, as Overview-Single has them", async () => {
    renderAt("/", { ...overviewApi(), updates });

    const card = await screen.findByRole("region", { name: "Updates" });
    expect(await within(card).findByText("1 security update for Ubuntu")).toBeVisible();
    const backups = screen.getByRole("region", { name: "Backups" });
    expect(card.parentElement).toBe(backups.parentElement);
    expect(card.parentElement?.lastElementChild).toBe(card);
    expect(screen.getByRole("region", { name: "Recent changes" }).parentElement).toBe(card.parentElement);
  });

  it("puts the updates right after the apps on a phone, as M-Overview-Single has them", async () => {
    onPhone();
    renderAt("/", { ...overviewApi(), updates });

    const link = await screen.findByRole("link", { name: /1 security update for Ubuntu/ });
    expect(link).toHaveAttribute("href", "/servers/srv_1/updates");
    const app = await screen.findByRole("link", { name: /^web/ });
    const changes = screen.getByRole("region", { name: "Recent changes" });
    expect(app.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(link.compareDocumentPosition(changes) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("lists the apps as rows on a phone, healthy ones quiet", async () => {
    onPhone();
    renderAt("/", overviewApi());

    const row = await screen.findByRole("link", { name: /^web/ });
    expect(row).toHaveAttribute("href", "/apps/svc_web");
    await waitFor(() => expect(within(row).getByText("Healthy")).toBeVisible());
    // Healthy reads in the line's quiet ink; only what needs a look takes its colour.
    expect(within(row).getByText("Healthy")).not.toHaveClass("dw-tone-healthy");
    const api = screen.getByRole("link", { name: /^api/ });
    expect(within(api).getByText("Unhealthy")).toHaveClass("dw-tone-warning");
  });
});

describe("the overview's backups", () => {
  const web = anApp("web", "ghcr.io/example/web:1.5.0");

  function backupsApi(backups: Api["backups"]): Api {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete);
    return {
      servers: { watchServers: watch.stream },
      services: { listServices: () => ({ services: [web] }) },
      resources: {
        listResources: (req: { kind: string }) =>
          req.kind === "volume"
            ? { resources: [{ id: "vol_1", resource: { kind: "volume", scope: ENV, name: "orders" } }] }
            : places().listResources(req),
      },
      health: running(web),
      domains: { listDomains: () => ({ domains: [] }) },
      events: { watchEvents: eventsWatch(), listEvents: () => ({}) },
      backups: {
        listBackups: () => ({ backups: [] }),
        listBackupPolicies: () => ({ policies: [] }),
        listBackupDestinations: () => ({ destinations: [] }),
        ...backups,
      },
    };
  }

  const nightly = (init: MessageInitShape<typeof BackupPolicyInfoSchema> = {}): BackupPolicyInfo =>
    create(BackupPolicyInfoSchema, {
      id: "pol_1",
      name: "orders-nightly",
      environmentId: ENV,
      spec: { volume: "orders", at: "03:00", timeZone: "Europe/Amsterdam", keep: 7 },
      nextRun: timestampFromDate(new Date("2026-11-08T02:00:00Z")),
      ...init,
    } as MessageInitShape<typeof BackupPolicyInfoSchema>);
  const done = create(BackupSchema, {
    id: "bkp_1",
    environmentId: ENV,
    volume: "orders",
    volumeId: "vol_1",
    state: BackupState.DONE,
    sizeBytes: 71n,
    startedAt: timestampFromDate(new Date(Date.now() - 600_000)),
    finishedAt: timestampFromDate(new Date(Date.now() - 596_000)),
  });
  const panel = () => screen.findByRole("region", { name: "Backups" });

  it("says nothing of them until every read has come", async () => {
    renderAt("/", backupsApi({ listBackups: () => new Promise(() => {}) }));
    const backups = await panel();
    expect(within(backups).getByText("Loading backups")).toBeInTheDocument();
    expect(backups).not.toHaveTextContent("Nothing to back up yet");
    expect(backups).not.toHaveTextContent("None yet");
  });

  it("says when it can't read them, and tries again", async () => {
    let calls = 0;
    renderAt(
      "/",
      backupsApi({
        listBackups: () => {
          calls += 1;
          if (calls === 1) throw new ConnectError("the control plane is restarting", Code.Unavailable);
          return { backups: [done] };
        },
      }),
    );
    const backups = await panel();
    expect(await within(backups).findByText("Dockwell can't read its backups")).toBeVisible();
    expect(backups).not.toHaveTextContent("Nothing to back up yet");
    await userEvent.click(within(backups).getByRole("button", { name: "Try again" }));
    expect(await within(backups).findByText(/orders · 71 B/)).toBeVisible();
  });

  it("says a schedule's last run failed", async () => {
    renderAt(
      "/",
      backupsApi({
        listBackupPolicies: () => ({
          policies: [nightly({ last: { id: "bkp_2", state: BackupState.FAILED, error: "disk full" } })],
        }),
      }),
    );
    expect(await within(await panel()).findByText("Last run failed")).toBeVisible();
  });

  it("says a schedule hasn't run yet, and when it will, in its own zone", async () => {
    renderAt("/", backupsApi({ listBackupPolicies: () => ({ policies: [nightly()] }) }));
    const backups = await panel();
    expect(await within(backups).findByText("Hasn't run yet")).toBeVisible();
    // 02:00 UTC is 03:00 in Amsterdam in November, and the zone is named.
    expect(within(backups).getByText(/03:00, Europe\/Amsterdam · orders/)).toBeVisible();
  });

  it("says where they're kept, a backup taken by hand included", async () => {
    const { unmount } = renderAt("/", backupsApi({ listBackups: () => ({ backups: [done] }) }));
    expect(await within(await panel()).findByText("On the server's own disk only")).toBeVisible();
    unmount();
    renderAt(
      "/",
      backupsApi({
        listBackups: () => ({ backups: [{ ...done, copy: BackupCopy.STORED, destination: "r2" }] }),
      }),
    );
    expect(await within(await panel()).findByText("On the server, and on r2")).toBeVisible();
  });
});

describe("the overview's apps, live", () => {
  it("shows an app made elsewhere, and where it answers, as soon as its change applies", async () => {
    const web = anApp("web", "ghcr.io/example/web:1.5.0");
    const cli = anApp("cli-made", "ghcr.io/example/worker:2.1.0");
    let made = false;
    let apply = () => {};
    const applying = new Promise<void>((resolve) => (apply = resolve));
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete);
    renderAt("/", {
      servers: { watchServers: watch.stream },
      services: { listServices: () => ({ services: made ? [web, cli] : [web] }) },
      resources: places(),
      health: running(web, cli),
      domains: {
        listDomains: () => ({
          domains: made
            ? [{ name: "cli.example.com", environmentId: ENV, routes: [{ service: "cli-made", port: 80 }] }]
            : [],
        }),
      },
      events: {
        listEvents: () => ({}),
        watchEvents: async function* (_req: unknown, ctx: HandlerContext): AsyncGenerator<WatchEventsResponse> {
          yield snapshot([]);
          await applying;
          yield since([changeEvent(applied(webPlanned()))]);
          await new Promise<void>((resolve) => ctx.signal.addEventListener("abort", () => resolve(), { once: true }));
        },
      },
      backups: { listBackups: () => ({}), listBackupPolicies: () => ({}), listBackupDestinations: () => ({}) },
    });
    const table = await screen.findByRole("table", { name: "Apps on this server" });
    await within(table).findByText("web");
    made = true;
    act(() => apply());
    expect(await within(table).findByText("cli-made", {}, { timeout: 3_000 })).toBeVisible();
    // Where it answers comes with it: the same change marks the domains' names old too.
    expect(await within(table).findByText("cli.example.com", {}, { timeout: 3_000 })).toBeVisible();
  });
});
