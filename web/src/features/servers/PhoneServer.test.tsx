// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  ActorKind,
  Connection,
  DeploymentState,
  EventKind,
  EventSchema,
  HealthState,
  ServerStrain,
  type WatchEventsResponse,
  WatchEventsResponseSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
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
  // Docker's bridge first, as a server's interfaces list it.
  inventory: {
    osName: "Ubuntu 24.04.1 LTS",
    cpuCount: 4,
    addresses: ["172.17.0.1", "10.0.0.2", "203.0.113.7"],
    outboundAddress: "10.0.0.2",
  },
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
    events: {
      watchEvents: events.stream,
      // A timeline of one server reads its own page, which the control plane filters.
      listEvents: (req) => (req.serverId === server.id ? { events: joined.events } : {}),
    },
    ...api,
  });
  await screen.findByRole("heading", { level: 1, name: "hz-1" });
  return rendered;
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

const minutesAgo = (n: number) => timestampFromDate(new Date(Date.now() - n * 60_000));

/** hz-1 past 90% of its memory since 13:58, as the control plane says once it has lasted. */
const strainedHz1 = () =>
  aServer({
    ...hz1,
    stats: sample(40, 7.4),
    strainedSince: timestampFromDate(new Date(2026, 10, 7, 13, 58)),
    strain: ServerStrain.MEMORY,
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
    await renderPhone(strainedHz1());
    const status = screen.getByRole("region", { name: "Status" });
    expect(status).toHaveTextContent("Memory above 90% since 13:58");
    expect(status).toHaveTextContent("Degraded");
    expect(status).not.toHaveTextContent("Healthy");
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

  it("gives as its private IP the address it goes out from, never Docker's bridge", async () => {
    await renderPhone();
    const status = screen.getByRole("region", { name: "Status" });
    expect(within(status).getByRole("button", { name: "Copy Private IP 10.0.0.2" })).toBeVisible();
    expect(status).not.toHaveTextContent("172.17.0.1");
  });

  it("says a VPS that goes out from its public address has no private IP", async () => {
    await renderPhone(
      aServer({
        id: "srv_1",
        name: "hz-1",
        stats: past.at(-1),
        history: past,
        inventory: { cpuCount: 4, addresses: ["172.17.0.1", "203.0.113.7"], outboundAddress: "203.0.113.7" },
        network: { ipv4: { address: "203.0.113.7" } },
      }),
    );
    const status = screen.getByRole("region", { name: "Status" });
    expect(status).toHaveTextContent("Private IPNone");
    expect(status).not.toHaveTextContent("172.17.0.1");
  });

  it("copies nothing, and throws nothing, in a browser with no clipboard", async () => {
    await renderPhone();
    vi.stubGlobal("navigator", { ...navigator, clipboard: undefined });
    const status = screen.getByRole("region", { name: "Status" });
    await userEvent.click(within(status).getByRole("button", { name: "Copy Public IP 203.0.113.7" }));
    expect(within(status).getByRole("button", { name: "Copy Public IP 203.0.113.7" })).toBeVisible();
  });

  it("says how long it hasn't reported, with the command that restarts its agent to copy", async () => {
    await renderPhone(aServer({ ...hz1, connection: Connection.UNREACHABLE, lastSeenTime: minutesAgo(5) }));
    const status = screen.getByRole("region", { name: "Status" });
    expect(status).toHaveTextContent("Not reporting for 5 min");
    expect(within(status).getByRole("button", { name: "Copy the restart command" })).toBeVisible();
  });

  it("says a server waits for its agent, with the command that starts it to copy", async () => {
    // Joining gives it a last-seen time; its agent hasn't spoken yet, so nothing reads as stopped or heard.
    await renderPhone(
      aServer({ ...hz1, connection: Connection.PENDING, stats: undefined, history: [], lastSeenTime: minutesAgo(6) }),
    );
    expect(screen.getByRole("region", { name: "Status" })).toHaveTextContent("Private IP10.0.0.2");
    const status = screen.getByRole("region", { name: "Status" });
    expect(status).toHaveTextContent("Waiting for its agent");
    expect(status).toHaveTextContent("Waiting");
    expect(status).not.toHaveTextContent("Stopped");
    expect(within(status).getByRole("button", { name: "Copy the start command" })).toBeVisible();
    expect(screen.getByRole("region", { name: "Agent" })).toHaveTextContent("Last heartbeatNever");
  });

  it("says how long Docker hasn't answered, with the command that restarts it to copy", async () => {
    await renderPhone(aServer({ ...hz1, dockerUnreadableSince: minutesAgo(10) }));
    const status = screen.getByRole("region", { name: "Status" });
    expect(status).toHaveTextContent("Docker hasn't answered for 10 min");
    expect(status).toHaveTextContent("Degraded");
    expect(within(status).getByRole("button", { name: "Copy the command that restarts Docker" })).toBeVisible();
  });

  it("says it can't say how the server is while the panel can't hear the control plane", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    await renderPhone();
    const status = screen.getByRole("region", { name: "Status" });
    expect(status).toHaveTextContent("Healthy");
    // Nothing heard, not even a heartbeat, for longer than what was heard can stand.
    await act(() => vi.advanceTimersByTimeAsync(35_000));
    await waitFor(() => expect(status).toHaveTextContent("Can't say how it is now"));
    expect(status).toHaveTextContent("Unknown");
    expect(status).not.toHaveTextContent("Healthy");
    // What it last said of its CPU and memory is greyed, not shown as now, and says how old it is.
    const cpu = within(status).getByRole("meter", { name: "CPU" });
    expect(cpu.closest("[aria-disabled]")).toHaveAttribute("aria-disabled", "true");
    expect(cpu).toHaveAttribute("aria-valuetext", expect.stringMatching(/^23%, as last heard \d+ s ago$/));
  });

  it("says Degraded in the header too, once the control plane says it's strained", async () => {
    await renderPhone(strainedHz1());
    expect(screen.getByText(/· up 3 days$/)).toHaveTextContent("Degraded · up 3 days");
    expect(screen.queryByText("Healthy · up 3 days")).toBeNull();
    expect(screen.getByRole("region", { name: "Status" })).toHaveTextContent("Degraded");
  });

  it("says None for a new agent's server with no route out and no private network", async () => {
    await renderPhone(
      aServer({
        id: "srv_1",
        name: "hz-1",
        stats: past.at(-1),
        history: past,
        inventory: { cpuCount: 4, addresses: ["203.0.113.7"], outboundAddress: "", addressesPhysical: true },
      }),
    );
    expect(screen.getByRole("region", { name: "Status" })).toHaveTextContent("Private IPNone");
  });

  it("rounds CPU down, so 89.6 doesn't read as 90 beside Healthy", async () => {
    await renderPhone(aServer({ ...hz1, stats: sample(89.6) }));
    const status = screen.getByRole("region", { name: "Status" });
    expect(within(status).getByRole("meter", { name: "CPU" })).toHaveAttribute("aria-valuetext", "89%");
    expect(status).toHaveTextContent("Healthy");
  });

  it("says its private IP isn't known until its agent says how it goes out", async () => {
    // One still waiting for its agent, and one from before agents said.
    await renderPhone(aServer({ id: "srv_1", name: "hz-1", connection: Connection.PENDING }));
    expect(screen.getByRole("region", { name: "Status" })).toHaveTextContent("Private IPNot known");
  });

  it("gives a cloud's private network as its private IP, once the agent lists only physical interfaces", async () => {
    const vps = (physical: boolean) =>
      aServer({
        id: "srv_1",
        name: "hz-1",
        stats: past.at(-1),
        history: past,
        inventory: {
          cpuCount: 4,
          // A custom Docker pool in 10.x would read the same in an older agent's list.
          addresses: ["203.0.113.7", "10.0.1.5"],
          outboundAddress: "203.0.113.7",
          addressesPhysical: physical,
        },
        network: { ipv4: { address: "203.0.113.7" } },
      });
    const { unmount } = await renderPhone(vps(true));
    expect(screen.getByRole("button", { name: "Copy Private IP 10.0.1.5" })).toBeVisible();
    unmount();
    await renderPhone(vps(false));
    expect(screen.getByRole("region", { name: "Status" })).toHaveTextContent("Private IPNone");
  });

  it("says an app failed to deploy here, from its deployment here, and one deploying", async () => {
    const at = timestampFromDate(new Date(Date.now() - 600_000));
    const on = (state: DeploymentState, serverId = "srv_1") => ({
      serverId,
      serverName: serverId,
      state,
      updateTime: at,
    });
    await renderPhone(hz1, {
      services: {
        listServices: () => ({
          services: [
            anApp("web", "nginx", {}, { deployments: [on(DeploymentState.FAILED)] }),
            anApp(
              "api",
              "ghcr.io/acme/api:2",
              {},
              { deployments: [on(DeploymentState.LIVE), on(DeploymentState.FAILED, "srv_2")] },
            ),
            anApp("worker", "ghcr.io/acme/worker:1", {}, { deployments: [on(DeploymentState.PENDING)] }),
          ],
        }),
      },
      health: {
        listHealth: () => ({
          services: [
            aHealth("svc_web", { state: HealthState.HEALTHY }),
            aHealth("svc_api", { state: HealthState.HEALTHY }),
            aHealth("svc_worker", { state: HealthState.HEALTHY }),
          ],
        }),
      },
    });
    const runs = screen.getByRole("region", { name: "Runs here" });
    await waitFor(() => expect(within(runs).getByRole("link", { name: /^web/ })).toHaveTextContent("Failed"));
    expect(within(runs).getByRole("link", { name: /^api/ })).toHaveTextContent("Healthy");
    expect(within(runs).getByRole("link", { name: /^worker/ })).toHaveTextContent("Deploying");
  });

  it("invites an app on a server with none", async () => {
    await renderPhone(hz1, { services: { listServices: () => ({ services: [] }) } });
    const runs = screen.getByRole("region", { name: "Runs here" });
    expect(await within(runs).findByRole("link", { name: /No apps here yet/ })).toHaveAttribute("href", "/apps/new");
  });
});
