// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { Connection, ServerInventorySchema, type WatchServersResponse } from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { expectMetric, metric } from "../../test/metrics";
import { feed, onPhone, renderAt } from "../../test/render";
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
    expectMetric("CPU", "23.4", "%", "Across 4 vCPU · last 15 s");
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

    await waitFor(() => expectMetric("CPU", "%", "Across 4 vCPU · last 5 min"));
    expect(screen.getByRole("img", { name: "CPU trend" })).toBeVisible();
  });

  it("draws the trend from the last gap in reporting, not across it", async () => {
    const { watch } = renderOverview();
    const before = history(80, 85, 90, 95);
    gap(2 * 60_000);
    const after = history(10, 11, 12);
    const all = [...before, ...after];
    watch.push(changed(aServer({ ...hz1, stats: all.at(-1), history: all })), snapshotComplete);

    await waitFor(() => expectMetric("CPU", "12.0", "%", "Across 4 vCPU · last 15 s"));
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

    // The page says so; the top bar's note only replaces the states it can no longer vouch for.
    const page = screen.getByRole("main");
    expect(await within(page).findByText("Reconnecting")).toBeVisible();
    expect(within(page).getByText("Reconnecting").closest("[role=status]")).not.toBeNull();
    expect(screen.getByText("23.4")).toBeVisible();
  });

  it("puts the server in the phone header and the numbers two by two", async () => {
    onPhone();
    const { watch, container } = renderOverview();
    watch.push(changed(hz1), snapshotComplete);

    expect(await screen.findByRole("heading", { level: 1, name: "hz-1" })).toBeVisible();
    expect(screen.getByText("Healthy · up 3 days")).toBeVisible();
    expectMetric("CPU", "23.4", "%");
    expect(screen.queryByRole("img", { name: "CPU trend" })).toBeNull();
    expect(screen.queryByText("Across 4 vCPU · last 15 s")).toBeNull();
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
  it("lists every server, each linked to its page", async () => {
    const { watch } = renderOverview();
    watch.push(changed(hz1), changed(aServer({ id: "srv_2", name: "hz-2", stats: sample(8) })), snapshotComplete);

    expect(await screen.findByText("2 servers", { selector: ".page-lead" })).toBeVisible();
    const list = screen.getByRole("region", { name: "Servers" });
    expect(within(list).getByRole("link", { name: "hz-1" })).toHaveAttribute("href", "/servers/srv_1");
    expect(within(list).getByRole("link", { name: "hz-2" })).toHaveAttribute("href", "/servers/srv_2");
    expect(screen.queryByText("Network")).toBeNull();
  });

  it("says once that it is reconnecting", async () => {
    const { watch } = renderOverview();
    watch.push(changed(hz1), changed(aServer({ id: "srv_2", name: "hz-2", stats: sample(8) })), snapshotComplete);
    await screen.findByText("2 servers", { selector: ".page-lead" });

    act(() => watch.fail(new ConnectError("the watch fell behind", Code.Unavailable)));

    const note = await within(screen.getByRole("main")).findByRole("status");
    expect(note).toHaveTextContent("Reconnecting");
    expect(screen.getAllByRole("status")).toEqual([note]);
    // The list keeps its servers, with no note of its own.
    expect(within(screen.getByRole("region", { name: "Servers" })).queryByText("Reconnecting")).toBeNull();
  });
});

describe("the server list page", () => {
  it("says once that it is reconnecting", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), changed(aServer({ id: "srv_2", name: "hz-2", stats: sample(8) })), snapshotComplete);
    renderAt("/servers", { servers: { watchServers: watch.stream } });
    await screen.findByRole("link", { name: "hz-2" });

    act(() => watch.fail(new ConnectError("the watch fell behind", Code.Unavailable)));

    const note = await within(screen.getByRole("main")).findByRole("status");
    expect(note).toHaveTextContent("Reconnecting");
    expect(screen.getAllByRole("status")).toEqual([note]);
  });
});
