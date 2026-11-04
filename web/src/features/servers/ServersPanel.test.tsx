// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";
import { Connection, type WatchServersResponse } from "@dockwell/api";
import { act, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { feed, renderWithApi } from "../../test/render";
import { aServer, changed, sample, snapshotComplete } from "../../test/servers";
import { ServersPanel } from "./ServersPanel";

function renderPanel(watch: ReturnType<typeof feed<WatchServersResponse>>) {
  return renderWithApi(<ServersPanel />, { servers: { watchServers: watch.stream } });
}

const controlServer = aServer({ id: "srv_1", name: "hz-1", controlServer: true, stats: sample(12.4, 2.4, 76) });
const pending = aServer({ id: "srv_2", name: "hz-2", connection: Connection.PENDING, inventory: undefined });

afterEach(() => {
  vi.useRealTimers();
});

describe("ServersPanel", () => {
  it("lists each server with its live stats", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(controlServer), changed(pending), snapshotComplete);
    renderPanel(watch);

    const panel = await screen.findByRole("region", { name: "Servers" });
    const [first, second] = await within(panel).findAllByRole("listitem");
    expect(first).toHaveTextContent("Healthy: hz-1");
    expect(within(first as HTMLElement).getByText("Control server")).toBeVisible();
    expect(first).toHaveTextContent("Ubuntu 24.04.1 LTS");
    expect(first).toHaveTextContent("CPU12%");
    expect(first).toHaveTextContent("Memory2.4 of 8 GB");
    // Uptime in the same words as the server's page: "up 3 days".
    expect(within(first as HTMLElement).getByText("Up").nextElementSibling).toHaveTextContent(/^3 days$/);

    expect(second).toHaveTextContent("Stopped: hz-2");
    expect(second).toHaveTextContent("Waiting for its agent");
    expect(second).toHaveTextContent("Unknown system");
    expect(within(second as HTMLElement).queryByText("CPU")).toBeNull();
  });

  it("follows changes as agents report them", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(controlServer), snapshotComplete);
    renderPanel(watch);
    expect(await screen.findByText("12%")).toBeVisible();

    act(() => watch.push(changed(aServer({ ...controlServer, stats: sample(57, 7.4, 0.5) }))));

    expect(await screen.findByText("57%")).toBeVisible();
    expect(screen.getByText("7.4 of 8 GB")).toBeVisible();
    expect(screen.getByText("30 min")).toBeVisible();
  });

  it("hides the stats of a server whose agent dropped", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(aServer({ ...controlServer, connection: Connection.UNREACHABLE })), snapshotComplete);
    renderPanel(watch);

    expect(await screen.findByText("Agent not connected")).toBeVisible();
    expect(screen.getByRole("listitem")).toHaveTextContent("Down: hz-1");
    expect(screen.queryByText("CPU")).toBeNull();
  });

  it("says how to add the first server", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(snapshotComplete);
    renderPanel(watch);

    const empty = await screen.findByText(/No servers yet/);
    expect(empty).toHaveTextContent("No servers yet. Add one with dockwell token create on this server.");
    expect(within(empty).getByText("dockwell token create").tagName).toBe("CODE");
  });

  it("says it is loading only when the list is slow", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderPanel(feed<WatchServersResponse>());
    // jsdom doesn't load stylesheets; is-quiet hides the message but holds its space.
    expect(screen.getByText("Loading servers")).toHaveClass("is-quiet");
    await act(() => vi.advanceTimersByTimeAsync(350));
    expect(screen.getByText("Loading servers")).not.toHaveClass("is-quiet");
  });

  it("keeps the list and says so while it reconnects", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(controlServer), snapshotComplete);
    renderPanel(watch);
    expect(await screen.findByText("12%")).toBeVisible();

    act(() => watch.fail(new ConnectError("the watch fell behind", Code.Unavailable)));

    expect(await screen.findByRole("status")).toHaveTextContent("Reconnecting");
    expect(screen.getByText("12%")).toBeVisible();
  });

  it("explains a list that didn't load", async () => {
    const watch = feed<WatchServersResponse>();
    watch.fail(new ConnectError("the fleet is still loading", Code.Unavailable));
    renderPanel(watch);

    expect(await screen.findByText("The server list didn't load. Dockwell keeps trying.")).toBeVisible();
    expect(screen.getByText("the fleet is still loading")).toBeVisible();
  });

  it("hands an ended session to the sign-in gate instead of retrying", async () => {
    const watchServers = vi.fn(() => {
      throw new ConnectError("your session ended", Code.Unauthenticated);
    });
    const getAuthState = vi.fn(() => ({}));
    renderWithApi(<ServersPanel />, { servers: { watchServers }, auth: { getAuthState } });
    await vi.waitFor(() => expect(watchServers).toHaveBeenCalledOnce());
    expect(screen.queryByText("your session ended")).toBeNull();
    await act(() => new Promise((resolve) => setTimeout(resolve, 50)));
    expect(watchServers).toHaveBeenCalledOnce();
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    const watch = feed<WatchServersResponse>();
    watch.push(changed(controlServer), snapshotComplete);
    renderPanel(watch);

    const row = await within(await screen.findByRole("region", { name: "服务器" })).findByRole("listitem");
    expect(row).toHaveTextContent("控制服务器");
    expect(row).toHaveTextContent("内存2.4 / 8 GB");
    expect(row).toHaveTextContent("运行时间3 天");
  });
});
