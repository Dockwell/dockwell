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
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, places, running } from "../../test/apps";
import { type Api, feed, onPhone, renderAt, untilAborted } from "../../test/render";
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

const rowOf = (name: RegExp) => screen.findByRole("row", { name });

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
    // Chosen once, into the URL, so it stays put as others change.
    await waitFor(() => expect(router.state.location.search).toEqual({ server: "srv_2" }));
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
    await waitFor(() => expect(screen.queryByRole("complementary")).toBeNull());
    // Closed stays closed, through a reload, and the keyboard is back on its row.
    expect(router.state.location.search).toEqual({ closed: true });
    await waitFor(() => expect(document.activeElement).toHaveTextContent(/^web-2/));
    expect(document.activeElement?.tagName).toBe("TR");
  });

  it("keeps the inspector it opened on its own when that server recovers, or another goes wrong", async () => {
    const watch = feed<WatchServersResponse>();
    const down = web2({ connection: Connection.UNREACHABLE });
    watch.push(changed(web1), changed(down), snapshotComplete);
    renderAt("/servers", {
      servers: { watchServers: watch.stream },
      services: { listServices: () => ({ services: [shop, blog] }) },
      resources: places(),
      health: running(shop, blog),
      events: { listEvents: () => ({ events: [] }) },
    });
    await screen.findByRole("complementary", { name: "web-2" });
    // web-2 comes back, and web-1, first by name, can't read Docker.
    act(() =>
      watch.push(changed(web2()), changed(aServer({ ...web1, dockerUnreadableSince: timestampFromDate(new Date()) }))),
    );
    await screen.findByRole("button", { name: /^web-2, Healthy/ });
    expect(screen.getByRole("complementary", { name: "web-2" })).toBeVisible();
    expect(screen.queryByRole("complementary", { name: "web-1" })).toBeNull();
  });

  it("goes back through the servers chosen", async () => {
    const user = userEvent.setup();
    const { router } = renderServers([web1, web2()]);
    await user.click(await screen.findByRole("button", { name: /^web-1, / }));
    await screen.findByRole("complementary", { name: "web-1" });
    await user.click(screen.getByRole("button", { name: /^web-2, / }));
    await screen.findByRole("complementary", { name: "web-2" });
    act(() => router.history.back());
    await waitFor(() => expect(router.state.location.search).toEqual({ server: "srv_1" }));
    expect(await screen.findByRole("complementary", { name: "web-1" })).toBeVisible();
  });

  it("opens nothing on its own once closed, the URL says so", async () => {
    renderServers([web1, web2({ connection: Connection.UNREACHABLE })], "/servers?closed=true");
    await screen.findByRole("button", { name: /^web-2, Down/ });
    expect(screen.queryByRole("complementary")).toBeNull();
  });

  it("shows no numbers in the inspector once the fleet reads Unknown", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let answering = true;
    renderAt("/servers?server=srv_1", {
      services: { listServices: () => ({ services: [shop, blog] }) },
      resources: places(),
      health: running(shop, blog),
      events: { listEvents: () => ({ events: [] }) },
      servers: {
        async *watchServers(_req: unknown, ctx: { signal: AbortSignal }) {
          if (answering) {
            yield changed(web1);
            yield changed(web2());
            yield snapshotComplete;
          }
          await untilAborted(ctx.signal);
        },
      },
    });
    const inspector = await screen.findByRole("complementary", { name: "web-1" });
    await waitFor(() => expect(within(inspector).getByText("CPU")).toBeVisible());
    answering = false;
    await vi.advanceTimersByTimeAsync(35_000);
    await waitFor(() => expect(inspector.querySelector(".dw-tone-unknown")).not.toBeNull());
    expect(within(inspector).queryByText("CPU")).toBeNull();
  });

  it("says why a server isn't well in its row, and which is the control server", async () => {
    renderServers([
      aServer({ ...web1, dockerUnreadableSince: timestampFromDate(new Date()) }),
      web2({ connection: Connection.PENDING }),
    ]);
    expect(await rowOf(/^web-1/)).toHaveTextContent("DegradedCan't read Docker · Control server");
    expect(await rowOf(/^web-2/)).toHaveTextContent("Waiting for its agent");
  });

  it("draws the inspector's history on an even clock across the agent's sampling paces", async () => {
    const past = [sample(10, 2, 1, 10_000), sample(20, 2, 1, 10_000), sample(30)];
    renderServers([aServer({ ...web1, history: past, stats: past.at(-1) })], "/servers?server=srv_1");
    const inspector = await screen.findByRole("complementary", { name: "web-1" });
    expect(within(inspector).getAllByText("Last 25 s")).toHaveLength(2);
  });

  it("pages the table as Servers-List does, and shows a server chosen past the page", async () => {
    const user = userEvent.setup();
    const many = Array.from({ length: 25 }, (_, i) =>
      aServer({
        id: `srv_${String(i).padStart(2, "0")}`,
        name: `node-${String(i).padStart(2, "0")}`,
        stats: sample(10),
      }),
    );
    renderServers(many);
    const table = await screen.findByRole("table", { name: "Servers, sorted by name" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(21));
    expect(screen.getByText("Showing 20 of 25 · sorted by name")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Show all 25" }));
    expect(within(table).getAllByRole("row")).toHaveLength(26);
    expect(screen.getByText("25 servers · sorted by name")).toBeVisible();
  });

  it.each(["/servers", "/servers?server=srv_24"])(
    "keeps a row beyond the first page mounted and returns focus when closed from %s",
    async (url) => {
      const user = userEvent.setup();
      const many = Array.from({ length: 25 }, (_, i) =>
        aServer({ id: `srv_${i}`, name: `node-${String(i).padStart(2, "0")}` }),
      );
      renderServers(many, url);
      await user.click(await screen.findByRole("button", { name: /^node-24, / }));
      const inspector = await screen.findByRole("complementary", { name: "node-24" });
      const table = screen.getByRole("table", { name: "Servers, sorted by name" });
      expect(within(table).getAllByRole("row")).toHaveLength(26);

      await user.click(within(inspector).getByRole("button", { name: "Close node-24" }));
      await waitFor(() => expect(screen.queryByRole("complementary")).toBeNull());
      expect(within(table).getAllByRole("row")).toHaveLength(26);
      expect(within(table).getByRole("row", { name: /^node-24/ })).toHaveFocus();

      await user.click(screen.getByRole("button", { name: /^node-00, / }));
      await screen.findByRole("complementary", { name: "node-00" });
      expect(within(table).getAllByRole("row")).toHaveLength(26);
    },
  );

  it("names apps two projects share once, with how many", async () => {
    const db = (scope: string) =>
      anApp("db", "postgres:16", {}, { id: `svc_db_${scope}`, resource: { kind: "service", scope, name: "db" } });
    renderServers([web1], "/servers", {
      services: { listServices: () => ({ services: [db("env_a"), db("env_b"), shop] }) },
    });
    expect(await rowOf(/^web-1/)).toHaveTextContent("db ×2, shop");
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

  it("says how to add a server, with the install script of the control plane's own release", async () => {
    const user = userEvent.setup();
    const asked: string[] = [];
    renderServers([web1, web2()], "/servers", {
      updates: {
        getServerUpdates: (req: { id: string }) => {
          asked.push(req.id);
          return { dockwell: { controlPlane: "0.2.1", releases: "https://github.com/Dockwell/dockwell/releases" } };
        },
      },
    });

    await user.click(await screen.findByRole("button", { name: "Add server" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a server" });
    expect(sheet).toHaveTextContent("Make a ticket on web-1");
    expect(within(sheet).getByText("dockwell token create")).toBeVisible();
    expect(
      await within(sheet).findByText(
        "curl -fsSL https://github.com/Dockwell/dockwell/releases/download/v0.2.1/install.sh | sudo sh -s -- --version 0.2.1 --join <ticket>",
      ),
    ).toBeVisible();
    expect(asked).toEqual(["srv_1"]);
    expect(sheet).not.toHaveTextContent("get.dockwell.dev");
    await user.click(within(sheet).getByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it.each(["0.2.1-dev", "v0.2.1-0.20261108120000-abcdef123456", "v0.2.1-next+abcdef1", "devel"])(
    "explains how to join manually on development build %s",
    async (version) => {
      const user = userEvent.setup();
      renderServers([web1, web2()], "/servers", {
        updates: {
          getServerUpdates: () => ({
            dockwell: { controlPlane: version, releases: "https://github.com/Dockwell/dockwell/releases" },
          }),
        },
      });
      await user.click(await screen.findByRole("button", { name: "Add server" }));
      const sheet = await screen.findByRole("dialog", { name: "Add a server" });
      await waitFor(() => expect(sheet).toHaveTextContent("development build with no release installer"));
      expect(sheet).toHaveTextContent("same source revision for the new server's architecture");
      expect(within(sheet).getByText("dockwell join <ticket>")).toBeVisible();
      expect(within(sheet).getByText("dockwell run", { selector: ".copy-command code" })).toBeVisible();
      expect(sheet).toHaveTextContent("same user, with sudo for both");
      expect(sheet).not.toHaveTextContent("curl");
      expect(sheet).not.toHaveTextContent("install script of the release");
    },
  );

  it("gives manual joining steps when installation metadata is unavailable", async () => {
    const user = userEvent.setup();
    renderServers([web1, web2()], "/servers", {
      updates: {
        getServerUpdates: () => ({
          dockwell: { controlPlane: "0.2.1", releases: "" },
        }),
      },
    });
    await user.click(await screen.findByRole("button", { name: "Add server" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a server" });
    await waitFor(() => expect(sheet).toHaveTextContent("The control server's installation details are unavailable"));
    expect(sheet).toHaveTextContent("Install an agent that matches its version");
    expect(within(sheet).getByText("dockwell join <ticket>")).toBeVisible();
    expect(sheet).not.toHaveTextContent("development build");
    expect(sheet).not.toHaveTextContent("curl");
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
