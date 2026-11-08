// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  BuildInfoSchema,
  CloudProvider,
  Connection,
  DeploymentState,
  EventKind,
  type Server,
  ServerInventorySchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, places, running } from "../../test/apps";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, history, sample, snapshotComplete } from "../../test/servers";

const GiB = 1024 ** 3;

const web1 = aServer({
  id: "srv_1",
  name: "web-1",
  controlServer: true,
  inventory: create(ServerInventorySchema, {
    osName: "Debian GNU/Linux 13",
    cpuCount: 4,
    memoryBytes: BigInt(8 * GiB),
    addresses: ["198.51.100.10", "10.0.0.10"],
  }),
  network: { provider: CloudProvider.HETZNER },
  agentBuild: create(BuildInfoSchema, { version: "0.2.1" }),
  stats: sample(23),
  history: history(20, 22, 23),
});

function web2(init: Parameters<typeof aServer>[0] = {}): Server {
  return aServer({
    id: "srv_2",
    name: "web-2",
    inventory: create(ServerInventorySchema, { osName: "Debian GNU/Linux 13", addresses: ["198.51.100.20"] }),
    stats: sample(70, 6),
    ...init,
  });
}

const shop = anApp(
  "shop",
  "ghcr.io/example/shop:1.0.0",
  {},
  {
    deployments: [
      { serverId: "srv_1", serverName: "web-1", state: DeploymentState.LIVE },
      { serverId: "srv_2", serverName: "web-2", state: DeploymentState.LIVE },
    ],
  },
);
const blog = anApp("blog", "ghost:5.96");

/** Servers, with these servers, the two apps, and an empty timeline. */
function renderServers(servers: Server[], url = "/servers", extra: Partial<Api> = {}) {
  const watch = feed<WatchServersResponse>();
  watch.push(...servers.map(changed), snapshotComplete);
  return renderAt(url, {
    servers: { watchServers: watch.stream },
    services: { listServices: () => ({ services: [shop, blog] }) },
    resources: places(),
    health: running(shop, blog),
    events: { listEvents: () => ({ events: [] }) },
    ...extra,
  });
}

afterEach(() => {
  vi.useRealTimers();
});

describe("Servers", () => {
  it("shows every server as a tile and a row, with its numbers and what runs there", async () => {
    renderServers([web1, web2()]);

    expect(await screen.findByText("2 servers", { selector: ".page-lead" })).toBeVisible();
    expect(screen.getByRole("button", { name: "web-2, Healthy, CPU 70%" })).toBeVisible();
    const table = screen.getByRole("table", { name: "Servers, sorted by name" });
    const [, first, second] = within(table).getAllByRole("row");
    expect(first).toHaveTextContent("web-1");
    expect(first).toHaveTextContent("198.51.100.10");
    expect(first).toHaveTextContent("Control server");
    expect(first).toHaveTextContent("23%");
    await waitFor(() => expect(first).toHaveTextContent("blog, shop"));
    expect(second).toHaveTextContent("70%");
    expect(second).toHaveTextContent("75%");
    expect(second).toHaveTextContent("shop");
    // A healthy fleet opens no server on its own.
    expect(screen.queryByRole("complementary")).toBeNull();
  });

  it("inspects the server that needs a look, says why, and closes", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(new Date(2026, 10, 8, 9, 30));
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    const lastSeenTime = timestampFromDate(new Date(2026, 10, 8, 9, 27));
    const { router } = renderServers([web1, web2({ connection: Connection.UNREACHABLE, lastSeenTime })]);

    const inspector = await screen.findByRole("complementary", { name: "web-2" });
    expect(inspector).toHaveTextContent("Downfor 3 min");
    expect(inspector).toHaveTextContent(
      "Its agent last reported 3 min ago. Check that dockwell run is still running on the server.",
    );
    expect(inspector).toHaveTextContent("Public IP198.51.100.20");
    // Its numbers are from before it stopped, so none are shown.
    expect(within(inspector).queryByText("CPU")).toBeNull();
    expect(within(inspector).getByRole("link", { name: "shop" })).toHaveAttribute("href", "/apps/svc_shop");
    expect(within(inspector).getByRole("link", { name: "Open web-2" })).toHaveAttribute("href", "/servers/srv_2");

    await user.click(within(inspector).getByRole("button", { name: "Close web-2" }));
    expect(screen.queryByRole("complementary")).toBeNull();
    expect(router.state.location.search).toEqual({});
  });

  it("inspects the server chosen from its tile or its row, and keeps it in the URL", async () => {
    const user = userEvent.setup();
    const asked: { kinds: EventKind[]; serverId: string }[] = [];
    const { router } = renderServers([web1, web2()], "/servers", {
      events: {
        listEvents: (req: { kinds: EventKind[]; serverId: string }) => {
          asked.push(req);
          return { events: [] };
        },
      },
    });

    await user.click(await screen.findByRole("button", { name: "web-1, Healthy, CPU 23%" }));
    const inspector = await screen.findByRole("complementary", { name: "web-1" });
    expect(router.state.location.search).toEqual({ server: "srv_1" });
    expect(inspector).toHaveTextContent("Private IP10.0.0.10");
    expect(inspector).toHaveTextContent("ProviderHetzner");
    expect(inspector).toHaveTextContent("Size4 vCPU · 8 GB");
    expect(inspector).toHaveTextContent("Agent0.2.1Control server");
    expect(within(inspector).getByText("CPU")).toBeVisible();
    expect(within(inspector).getAllByText("Last 15 s")).toHaveLength(2);
    // What happened there, not who read its files.
    await waitFor(() =>
      expect(asked).toContainEqual(
        expect.objectContaining({
          serverId: "srv_1",
          kinds: [EventKind.CHANGE, EventKind.SERVER, EventKind.HEALTH, EventKind.BACKUP],
        }),
      ),
    );

    const table = screen.getByRole("table", { name: "Servers, sorted by name" });
    await user.click(within(table).getByText("web-2"));
    expect(await screen.findByRole("complementary", { name: "web-2" })).toBeVisible();
    expect(router.state.location.search).toEqual({ server: "srv_2" });
  });

  it("opens the server the URL names", async () => {
    renderServers([web1, web2()], "/servers?server=srv_2");
    expect(await screen.findByRole("complementary", { name: "web-2" })).toBeVisible();
  });

  it("says how to add a server with the commands that work today", async () => {
    const user = userEvent.setup();
    renderServers([web1, web2()]);

    await user.click(await screen.findByRole("button", { name: "Add server" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a server" });
    expect(sheet).toHaveTextContent("Make a ticket on web-1");
    expect(within(sheet).getByText("sudo dockwell token create")).toBeVisible();
    expect(within(sheet).getByText("curl -fsSL https://get.dockwell.dev | sh -s -- --join <ticket>")).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("lists what needs attention on a phone, and holds the server chosen with the way to its page", async () => {
    onPhone();
    const user = userEvent.setup();
    renderServers([web1, web2({ connection: Connection.UNREACHABLE })]);

    const attention = await screen.findByRole("region", { name: "Needs attention" });
    const row = within(attention).getByRole("button", { name: /web-2/ });
    expect(row).toHaveAttribute("aria-pressed", "true");
    expect(row).toHaveTextContent("Down · Agent not connected");
    expect(within(attention).queryByRole("button", { name: /web-1/ })).toBeNull();
    const chosen = screen.getByRole("region", { name: "Chosen server" });
    expect(within(chosen).getByRole("link", { name: "Open" })).toHaveAttribute("href", "/servers/srv_2");

    await user.click(screen.getByRole("button", { name: "web-1, Healthy, CPU 23%" }));
    expect(
      within(screen.getByRole("region", { name: "Chosen server" })).getByRole("link", { name: "Open" }),
    ).toHaveAttribute("href", "/servers/srv_1");
  });

  it("passes axe, wide and on a phone", async () => {
    const wide = renderServers([web1, web2({ connection: Connection.UNREACHABLE })]);
    await screen.findByRole("complementary", { name: "web-2" });
    await expectAccessible(wide.container);
    wide.unmount();

    onPhone();
    const phone = renderServers([web1, web2({ connection: Connection.UNREACHABLE })]);
    await screen.findByRole("region", { name: "Chosen server" });
    await expectAccessible(phone.container);
  });
});
