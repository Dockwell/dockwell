// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  ActorKind,
  EventKind,
  EventSchema,
  HealthState,
  type WatchEventsResponse,
  WatchEventsResponseSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { aHealth, anApp } from "../../test/apps";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, history, sample, snapshotComplete } from "../../test/servers";

const past = history(20, 22, 23.4);
const hz1 = aServer({
  id: "srv_1",
  name: "hz-1",
  stats: past.at(-1),
  history: past,
  inventory: { osName: "Ubuntu 24.04.1 LTS", cpuCount: 4, addresses: ["10.0.0.2", "203.0.113.7"] },
  network: { ipv4: { address: "203.0.113.7" } },
});

const joined = create(WatchEventsResponseSchema, {
  snapshot: true,
  events: [
    create(EventSchema, {
      id: "evt_1",
      kind: EventKind.SERVER,
      action: "server.join",
      time: timestampFromDate(new Date(Date.now() - 3_600_000)),
      actor: { kind: ActorKind.DOCKWELL, name: "Dockwell" },
      subject: { id: "srv_1", name: "hz-1" },
    }),
  ],
});

async function renderPhone(server = hz1, api: Api = {}) {
  onPhone();
  const servers = feed<WatchServersResponse>();
  servers.push(changed(server), snapshotComplete);
  const events = feed<WatchEventsResponse>();
  events.push(joined);
  const rendered = renderAt("/servers/srv_1", {
    servers: { watchServers: servers.stream, getServerDiskUse: () => ({}) },
    events: { watchEvents: events.stream, listEvents: () => ({}) },
    ...api,
  });
  await screen.findByRole("heading", { level: 1, name: "hz-1" });
  return rendered;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("a server on a phone", () => {
  it("leads with how it is, its CPU and memory, and its addresses to copy", async () => {
    const writeText = vi.fn(async () => {});
    await renderPhone();
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    const status = screen.getByRole("region", { name: "Status" });
    expect(status).toHaveTextContent("Healthy");
    expect(status).toHaveTextContent("Up 3 days");
    expect(within(status).getByRole("meter", { name: "CPU" })).toHaveAttribute("aria-valuetext", "23%");
    expect(within(status).getByRole("meter", { name: "Memory" })).toHaveAttribute("aria-valuetext", "2.4 of 8 GB");
    await userEvent.click(within(status).getByRole("button", { name: "Copy Public IP 203.0.113.7" }));
    expect(writeText).toHaveBeenCalledWith("203.0.113.7");
    expect(await within(status).findByRole("button", { name: "Copied 203.0.113.7" })).toBeVisible();
    expect(within(status).getByRole("button", { name: "Copy Private IP 10.0.0.2" })).toBeVisible();
  });

  it("says what's wrong first, and draws memory past 90% as a warning", async () => {
    await renderPhone(aServer({ ...hz1, stats: sample(40, 7.4) }));
    const status = screen.getByRole("region", { name: "Status" });
    expect(status).toHaveTextContent("Memory above 90%");
    const memory = within(status).getByRole("meter", { name: "Memory" });
    expect(memory.querySelector("span")).toHaveClass("is-high");
  });

  it("lists what runs here, with how many copies, its revision and how it is", async () => {
    await renderPhone(hz1, {
      services: {
        listServices: () => ({ services: [anApp("worker", "ghcr.io/acme/worker:1"), anApp("web", "nginx")] }),
      },
      health: {
        listHealth: () => ({
          services: [
            aHealth("svc_worker", { state: HealthState.UNHEALTHY }),
            aHealth("svc_web", { state: HealthState.HEALTHY }),
          ],
        }),
      },
    });
    const runs = screen.getByRole("region", { name: "Runs here" });
    await waitFor(() => expect(runs).toHaveTextContent("2 apps · 2 replicas"));
    const worker = within(runs).getByRole("link", { name: /^worker/ });
    expect(worker).toHaveTextContent("worker×1r3Degraded");
    expect(worker).toHaveAttribute("href", "/apps/svc_worker");
  });

  it("says what happened lately, with the way to all of it", async () => {
    await renderPhone();
    const recent = screen.getByRole("region", { name: "Recent" });
    expect(await within(recent).findByText(/hz-1 joined/)).toBeVisible();
    expect(within(recent).getByRole("link", { name: "All activity" })).toHaveAttribute(
      "href",
      "/activity?server=srv_1",
    );
  });

  it("passes axe, and speaks Simplified Chinese", async () => {
    const { container, unmount } = await renderPhone();
    await screen.findByText(/hz-1 joined/);
    await expectAccessible(container);
    unmount();
    await activateLocale("zh-CN");
    await renderPhone();
    expect(screen.getByRole("region", { name: "状态" })).toHaveTextContent("已运行 3 天");
    expect(screen.getByRole("region", { name: "在此运行" })).toBeVisible();
    expect(screen.getByRole("region", { name: "最近" })).toBeVisible();
  });
});
