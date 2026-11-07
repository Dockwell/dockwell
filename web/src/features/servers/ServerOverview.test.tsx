// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  BuildInfoSchema,
  Connection,
  DiskSchema,
  ServerInventorySchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { expectMetric } from "../../test/metrics";
import { feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, history, sample, snapshotComplete } from "../../test/servers";

const GiB = 1024 ** 3;
const NOW = new Date(2026, 10, 4, 12, 0);

const inventory = create(ServerInventorySchema, {
  hostname: "hz-1",
  osName: "Ubuntu 24.04.1 LTS",
  kernel: "6.8.0-48-generic",
  architecture: "amd64",
  cpuModel: "AMD EPYC 7763 64-Core Processor",
  cpuCount: 4,
  memoryBytes: BigInt(8 * GiB),
  disks: [
    create(DiskSchema, { mountPoint: "/", device: "/dev/sda1", filesystem: "ext4", sizeBytes: BigInt(160 * GiB) }),
  ],
  dockerVersion: "27.3.1",
  addresses: ["10.0.0.2", "49.12.33.7", "2a01:4f8:c012:9a3e::1", "172.17.0.1"],
});
const past = history(20, 22, 23.4);
const hz1 = aServer({
  id: "srv_1",
  name: "hz-1",
  controlServer: true,
  inventory,
  stats: past.at(-1),
  history: past,
  agentBuild: create(BuildInfoSchema, { version: "v0.4.0" }),
  createTime: timestampFromDate(new Date(2026, 8, 24, 9, 30)),
  connectTime: timestampFromDate(new Date(2026, 10, 4, 9, 0)),
  lastSeenTime: timestampFromDate(new Date(2026, 10, 4, 11, 59, 58)),
});

/** Renders the server's page at path once the snapshot holds servers. */
async function renderServer(path: string, ...servers: ReturnType<typeof aServer>[]) {
  const watch = feed<WatchServersResponse>();
  watch.push(...servers.map(changed), snapshotComplete);
  const rendered = renderAt(path, { servers: { watchServers: watch.stream } });
  await screen.findByRole("heading", { level: 1 });
  return { ...rendered, watch };
}

const panel = (name: string) => screen.getByRole("region", { name });

afterEach(() => {
  vi.useRealTimers();
});

describe("ServerOverview", () => {
  it("names the server, its role and where it is", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    await renderServer("/servers/srv_1", hz1);
    expect(await screen.findByRole("heading", { level: 1, name: "hz-1" })).toBeVisible();
    expect(screen.getByText("Runs the control plane")).toBeVisible();
    expect(screen.getByText("up 3 days", { exact: false })).toHaveTextContent("49.12.33.7 · up 3 days");
    expect(document.title).toBe("hz-1 · Dockwell");
    expect(screen.getByRole("link", { name: "Server hz-1" })).toHaveAttribute("aria-current", "page");
  });

  it("names a hostname that is the address once", async () => {
    const errors = vi.spyOn(console, "error");
    const named = aServer({
      ...hz1,
      inventory: create(ServerInventorySchema, { ...inventory, hostname: "49.12.33.7" }),
    });
    await renderServer("/servers/srv_1", named);
    const lead = screen.getByText("up 3 days", { exact: false });
    expect(lead).toHaveTextContent(/^49\.12\.33\.7 · up 3 days$/);
    expect(lead.querySelectorAll(".server-address")).toHaveLength(1);
    expect(errors.mock.calls.flat().join(" ")).not.toContain("same key");
  });

  it("tags a private address shown as the server's", async () => {
    const behindNAT = aServer({ ...hz1, inventory: create(ServerInventorySchema, { addresses: ["10.0.0.25"] }) });
    await renderServer("/servers/srv_1", behindNAT);
    await screen.findAllByText("10.0.0.25");
    expect(document.querySelector(".server-address")).toHaveTextContent(/^10\.0\.0\.25Private$/);
    expect(document.querySelector(".topbar-address")).toHaveTextContent(/^10\.0\.0\.25Private$/);
  });

  it("tags a TUN's address, as it's no way in", async () => {
    const tunnelled = aServer({
      ...hz1,
      inventory: create(ServerInventorySchema, { addresses: ["198.18.0.1"] }),
    });
    await renderServer("/servers/srv_1", tunnelled);
    await screen.findAllByText("198.18.0.1");
    expect(document.querySelector(".server-address")).toHaveTextContent(/^198\.18\.0\.1Private$/);
  });

  it("shows live CPU, memory, disk and load", async () => {
    await renderServer("/servers/srv_1", hz1);
    await screen.findByText("Load average");
    expectMetric("CPU", "23.4", "%");
    expectMetric("Memory", "2.4", "of 8 GB");
    expectMetric("Disk", "61", "of 160 GB", "99 GB free");
    expectMetric("Load average", "0.84", "/ 0.91 / 0.88", "1, 5 and 15 min, on 4 vCPU");
    expect(screen.queryByText("Network", { selector: ".dw-metric-label span" })).toBeNull();
  });

  it("draws each disk's used and free space", async () => {
    await renderServer("/servers/srv_1", hz1);
    const disks = await screen.findByRole("region", { name: "Disks" });
    expect(within(disks).getByText("/")).toBeVisible();
    expect(disks).toHaveTextContent("/dev/sda1 · ext4");
    expect(disks).toHaveTextContent("38% of 160 GB used");
    expect(within(disks).getByRole("img", { name: "/: 61 GB used, 99 GB free, of 160 GB" })).toBeVisible();
    expect(disks).toHaveTextContent("Used61 GBFree99 GB");
  });

  it("says what the server is", async () => {
    await renderServer("/servers/srv_1", hz1);
    const system = await screen.findByRole("region", { name: "System" });
    expect(system).toHaveTextContent("amd64");
    expect(system).toHaveTextContent("Operating systemUbuntu 24.04.1 LTS");
    expect(system).toHaveTextContent("Kernel6.8.0-48-generic");
    expect(system).toHaveTextContent("ProcessorAMD EPYC 7763 64-Core Processor · 4 vCPU");
    expect(system).toHaveTextContent("Memory8 GB");
    expect(system).toHaveTextContent("Docker27.3.1");
    expect(system).toHaveTextContent("Hostnamehz-1");
  });

  it("lists public addresses to copy, and private ones apart", async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    await renderServer("/servers/srv_1", hz1);
    const network = await screen.findByRole("region", { name: "Network" });
    expect(network).toHaveTextContent("12 Mbit/s now");
    expect(network).toHaveTextContent("Public IPv449.12.33.7");
    expect(network).toHaveTextContent("Public IPv62a01:4f8:c012:9a3e::1");
    expect(network).toHaveTextContent("Private addresses10.0.0.2172.17.0.1");
    expect(network).toHaveTextContent("Receiving8 Mbit/sSending4.4 Mbit/s");

    await userEvent.click(within(network).getByRole("button", { name: "Copy 49.12.33.7" }));
    expect(writeText).toHaveBeenCalledWith("49.12.33.7");
    expect(await within(network).findByRole("button", { name: "Copied 49.12.33.7" })).toBeVisible();
  });

  it("follows the agent: its build, its connection and its heartbeat", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    await renderServer("/servers/srv_1", hz1);
    const agent = await screen.findByRole("region", { name: "Agent" });
    expect(agent).toHaveTextContent("Connected");
    expect(await within(agent).findByText("· same as the control plane")).toBeVisible();
    expect(agent).toHaveTextContent("Versionv0.4.0");
    expect(agent).toHaveTextContent("Control planeRuns on this server");
    expect(agent).toHaveTextContent("ConnectedFor 3 hours");
    expect(agent).toHaveTextContent("Last heartbeat2 s ago");
    expect(agent).toHaveTextContent("Joined24 Sept 2026, 09:30");

    await act(() => vi.advanceTimersByTimeAsync(3_000));
    expect(agent).toHaveTextContent("Last heartbeat5 s ago");
  });

  it("measures the agent's times on the control plane's clock, not the browser's", async () => {
    // The control plane's clock is 10 min behind the browser's.
    const skew = 10 * 60_000;
    const controlNow = () => new Date(Date.now() - skew);
    const ago = (ms: number) => timestampFromDate(new Date(controlNow().getTime() - ms));
    const server = aServer({ ...hz1, connectTime: ago(30 * 60_000), lastSeenTime: ago(3_000) });
    const { watch } = await renderServer("/servers/srv_1", server);

    // A sample arrives, stamped by the control plane as it hears it.
    act(() => watch.push(changed(aServer({ ...server, history: [], stats: sample(40), lastSeenTime: ago(0) }))));

    const agent = await screen.findByRole("region", { name: "Agent" });
    await vi.waitFor(() => expect(agent).toHaveTextContent("ConnectedFor 30 min"));
    expect(agent).toHaveTextContent("Last heartbeat1 s ago");
  });

  it("says when the agent's build differs from the control plane's", async () => {
    const hz2 = aServer({
      id: "srv_2",
      name: "hz-2",
      stats: sample(5),
      agentBuild: create(BuildInfoSchema, { version: "v0.3.9" }),
    });
    await renderServer("/servers/srv_2", hz1, hz2);
    const agent = await screen.findByRole("region", { name: "Agent" });
    expect(await within(agent).findByText("· the control plane runs v0.4.0")).toBeVisible();
    expect(agent).toHaveTextContent("Control planeRuns on another server");
  });

  it("explains a server whose agent dropped, and keeps what it knew", async () => {
    await renderServer(
      "/servers/srv_1",
      aServer({ ...hz1, connection: Connection.UNREACHABLE, connectTime: undefined }),
    );
    expect(await screen.findByRole("alert")).toHaveTextContent("hz-1 isn't reporting");
    const agent = screen.getByRole("region", { name: "Agent" });
    expect(agent).toHaveTextContent("Not connected");
    expect(agent).not.toHaveTextContent("For ");
    expect(screen.getByRole("region", { name: "Network" })).not.toHaveTextContent("Receiving");
    expect(screen.getByRole("region", { name: "System" })).toHaveTextContent("Ubuntu 24.04.1 LTS");
  });

  it("explains a server that hasn't reported yet", async () => {
    await renderServer(
      "/servers/srv_1",
      aServer({ id: "srv_1", name: "hz-1", connection: Connection.PENDING, inventory: undefined }),
    );
    expect(await screen.findByText("hz-1 is waiting for its agent")).toBeVisible();
    expect(screen.getByRole("region", { name: "System" })).toHaveTextContent(
      "The agent hasn't said what this server is yet.",
    );
    expect(screen.getByRole("region", { name: "Agent" })).toHaveTextContent("Waiting");
    expect(screen.getByRole("region", { name: "Agent" })).toHaveTextContent("Last heartbeatNever");
    expect(screen.queryByRole("region", { name: "Disks" })).toBeNull();
  });

  it("says when there's no such server", async () => {
    await renderServer("/servers/srv_9", hz1);
    expect(await screen.findByRole("heading", { level: 2, name: "No server with this ID" })).toBeVisible();
    expect(screen.getByText("srv_9").tagName).toBe("CODE");
    expect(within(screen.getByRole("main")).getByRole("link", { name: "Go to the overview" })).toHaveAttribute(
      "href",
      "/",
    );
  });

  it("is reached from the server list in a fleet, with a way back on phones", async () => {
    onPhone();
    const user = userEvent.setup();
    await renderServer("/servers", hz1, aServer({ id: "srv_2", name: "hz-2", stats: sample(8) }));
    await user.click(screen.getByRole("link", { name: "hz-2" }));

    expect(await screen.findByRole("heading", { level: 1, name: "hz-2" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/servers");
    const tabs = screen.getByRole("navigation", { name: "Main" });
    expect(within(tabs).getByRole("link", { name: "Servers" })).toHaveAttribute("aria-current", "page");
  });

  it("passes axe, wide and on a phone", async () => {
    const { container, unmount } = await renderServer("/servers/srv_1", hz1);
    await screen.findByRole("region", { name: "Agent" });
    await expectAccessible(container);
    unmount();

    onPhone();
    const phone = await renderServer("/servers/srv_1", hz1);
    expect(screen.getByRole("heading", { level: 1, name: "hz-1" })).toBeVisible();
    expect(screen.getByText("Healthy · up 3 days")).toBeVisible();
    await expectAccessible(phone.container);
  });

  it("speaks Simplified Chinese", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    await activateLocale("zh-CN");
    await renderServer("/servers/srv_1", hz1);
    expect(await screen.findByText("运行控制平面")).toBeVisible();
    expectMetric("平均负载", "0.84", "1、5 和 15 分钟，共 4 个 vCPU");
    expect(panel("磁盘")).toHaveTextContent("160 GB 中已用 38%");
    expect(panel("系统")).toHaveTextContent("操作系统Ubuntu 24.04.1 LTS");
    expect(panel("网络")).toHaveTextContent("公网 IPv449.12.33.7");
    expect(panel("代理")).toHaveTextContent("已连接 3 小时");
    expect(panel("代理")).toHaveTextContent("最后心跳2 秒前");
  });
});
