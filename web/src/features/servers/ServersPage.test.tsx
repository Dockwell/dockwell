// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  BuildInfoSchema,
  ChangeSchema,
  ChangeState,
  CloudProvider,
  Connection,
  type CreateJoinTicketRequest,
  DeploymentState,
  EventKind,
  FieldOp,
  JoinReach,
  JoinTicketState,
  PlanAction,
  type PlanServerLabelsRequest,
  type Server,
  ServerInventorySchema,
  ServerStrain,
  StepUpAction,
  Surface,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, places, running } from "../../test/apps";
import { authError, stubPasskeys } from "../../test/auth";
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
function renderServers(servers: Server[], url = "/servers", { servers: more, ...extra }: Partial<Api> = {}) {
  const watch = feed<WatchServersResponse>();
  watch.push(...servers.map(changed), snapshotComplete);
  return renderAt(url, {
    servers: { watchServers: watch.stream, ...more },
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

  it("opens on the server that needs a look first, as the Overview names it: a degraded one before one waiting", async () => {
    // By name, the one waiting for its agent comes first; by need, the live one that can't read Docker does.
    const { router } = renderServers([
      web1,
      aServer({ id: "srv_a", name: "a-waits", connection: Connection.PENDING }),
      web2({ dockerUnreadableSince: timestampFromDate(new Date()) }),
    ]);
    await waitFor(() => expect(router.state.location.search).toEqual({ server: "srv_2" }));
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
    expect(inspector).toHaveTextContent("Last heard 3 min ago.");
    expect(within(inspector).getByRole("button", { name: "Copy the restart command" })).toBeVisible();
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

  it("draws no map for one server, wide or on a phone: its row says it all", async () => {
    const wide = renderServers([web1]);
    expect(await rowOf(/^web-1/)).toHaveTextContent("Healthy");
    expect(screen.queryByRole("region", { name: "Servers at a glance" })).toBeNull();
    wide.unmount();

    onPhone();
    renderServers([web1]);
    expect(await screen.findByRole("button", { name: /web-1/ })).toBeVisible();
    expect(screen.queryByRole("region", { name: "Servers at a glance" })).toBeNull();
  });

  it("draws the map from two servers on", async () => {
    renderServers([web1, web2()]);
    expect(await screen.findByRole("region", { name: "Servers at a glance" })).toBeVisible();
  });

  it("counts a strained server as needing a look, and says why in its row and its inspector", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(new Date(2026, 10, 8, 14, 1));
    const strainedSince = timestampFromDate(new Date(2026, 10, 8, 13, 58));
    const { router } = renderServers([web1, web2({ strainedSince, strain: ServerStrain.MEMORY })]);

    // Its badge says Degraded, so it's the one the page opens, as for any server that isn't well.
    const inspector = await screen.findByRole("complementary", { name: "web-2" });
    await waitFor(() => expect(router.state.location.search).toEqual({ server: "srv_2" }));
    expect(inspector).toHaveTextContent("Degradedfor 3 min");
    expect(inspector).toHaveTextContent("Memory above 90% since 13:58");
    expect(await rowOf(/^web-2/)).toHaveTextContent("DegradedMemory above 90%");
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

  describe("adding a server", () => {
    const release = {
      updates: {
        getServerUpdates: () => ({
          dockwell: { controlPlane: "0.2.1", releases: "https://github.com/Dockwell/dockwell/releases" },
        }),
      },
    };
    const inAnHour = () => timestampFromDate(new Date(Date.now() + 3_600_000));
    /** The servers API with a ticket maker that wants a passkey, and a ticket that reads as these states in turn. */
    function tickets(
      reach: JoinReach,
      states: { state?: JoinTicketState; serverId?: string; serverName?: string }[],
      expireTime = inAnHour(),
    ) {
      const made: CreateJoinTicketRequest[] = [];
      const ticket = {
        id: "jtk_1",
        state: JoinTicketState.WAITING,
        expireTime,
        addresses: [{ url: "https://203.0.113.7:7443", reach }],
      };
      let read = 0;
      return {
        made,
        servers: {
          createJoinTicket: (req: CreateJoinTicketRequest) => {
            made.push(req);
            if (!req.proof) throw authError(Code.PermissionDenied, "needs_step_up", "this needs your passkey just now");
            return { ticket, value: "TICKET" };
          },
          getJoinTicket: () => ({ ticket: { ...ticket, ...states[Math.min(read++, states.length - 1)] } }),
        },
      };
    }
    const passkey = (steps: StepUpAction[]) => ({
      auth: {
        beginStepUp: (req: { action: StepUpAction }) => {
          steps.push(req.action);
          return { ceremonyId: "cer_1", optionsJson: JSON.stringify({ challenge: "AAAA", allowCredentials: [] }) };
        },
        finishStepUp: () => ({ proof: "proof-1" }),
      },
    });

    it("makes the command with a passkey, then says when the server joins, with the way to it", async () => {
      stubPasskeys();
      const user = userEvent.setup();
      const steps: StepUpAction[] = [];
      const api = tickets(JoinReach.ANYWHERE, [
        {},
        { state: JoinTicketState.JOINED, serverId: "srv_3", serverName: "web-3" },
      ]);
      renderServers([web1, web2()], "/servers", { ...release, ...passkey(steps), servers: api.servers });

      await user.click(await screen.findByRole("button", { name: "Add server" }));
      const sheet = await screen.findByRole("dialog", { name: "Add a server" });
      expect(sheet).toHaveTextContent("Nothing on web-1 changes.");
      expect(sheet).not.toHaveTextContent("dockwell token create");
      await user.click(within(sheet).getByRole("button", { name: "Make the command" }));

      expect(
        await within(sheet).findByText(
          "curl -fsSL https://github.com/Dockwell/dockwell/releases/download/v0.2.1/install.sh | sudo sh -s -- --version 0.2.1 --join TICKET",
        ),
      ).toBeVisible();
      expect(steps).toEqual([StepUpAction.ADD_SERVER]);
      expect(api.made.map((r) => r.proof)).toEqual(["", "proof-1"]);
      expect(sheet).toHaveTextContent("Waiting for the server to join");
      expect(sheet).toHaveTextContent("Works once · expires in 59 min");
      expect(sheet).toHaveTextContent("It reaches web-1 at 203.0.113.7:7443.");
      expect(within(sheet).queryByRole("button", { name: "Set public address" })).toBeNull();

      expect(await within(sheet).findByText("web-3 joined, starting its agent", {}, { timeout: 5000 })).toBeVisible();
      expect(within(sheet).getByRole("link", { name: "Open web-3" })).toHaveAttribute("href", "/servers/srv_3");
      expect(sheet).not.toHaveTextContent("TICKET");
      await expectAccessible(sheet);
    });

    it("says only the control server's network can join through a private address, with the fix beside it", async () => {
      stubPasskeys();
      const user = userEvent.setup();
      const api = tickets(JoinReach.SAME_NETWORK, [{}]);
      renderServers([web1, web2()], "/servers", { ...release, ...passkey([]), servers: api.servers });

      await user.click(await screen.findByRole("button", { name: "Add server" }));
      const sheet = await screen.findByRole("dialog", { name: "Add a server" });
      await user.click(within(sheet).getByRole("button", { name: "Make the command" }));
      expect(await within(sheet).findByText("Only servers on web-1's network can join")).toBeVisible();
      expect(sheet).toHaveTextContent("It joins through 203.0.113.7:7443, a private address.");
      await user.click(within(sheet).getByRole("button", { name: "Set public address" }));
      expect(await screen.findByRole("dialog", { name: "Set the public address of web-1" })).toBeVisible();
    });

    it("gives the join and run commands, with the ticket in place, where the build has no installer", async () => {
      stubPasskeys();
      const user = userEvent.setup();
      const api = tickets(JoinReach.ANYWHERE, [{}]);
      renderServers([web1, web2()], "/servers", {
        ...passkey([]),
        servers: api.servers,
        updates: {
          getServerUpdates: () => ({
            dockwell: { controlPlane: "0.2.1-dev", releases: "https://github.com/Dockwell/dockwell/releases" },
          }),
        },
      });
      await user.click(await screen.findByRole("button", { name: "Add server" }));
      const sheet = await screen.findByRole("dialog", { name: "Add a server" });
      await user.click(within(sheet).getByRole("button", { name: "Make the command" }));
      expect(await within(sheet).findByText("dockwell join TICKET")).toBeVisible();
      expect(within(sheet).getByText("dockwell run", { selector: ".copy-command code" })).toBeVisible();
      expect(sheet).toHaveTextContent("Install the agent of 0.2.1-dev as dockwell first");
      expect(sheet).not.toHaveTextContent("curl");
    });

    it("offers a new command once the ticket runs out", async () => {
      stubPasskeys();
      const user = userEvent.setup();
      const api = tickets(JoinReach.ANYWHERE, [{}], timestampFromDate(new Date(Date.now() - 1000)));
      renderServers([web1, web2()], "/servers", { ...release, ...passkey([]), servers: api.servers });
      await user.click(await screen.findByRole("button", { name: "Add server" }));
      const sheet = await screen.findByRole("dialog", { name: "Add a server" });
      await user.click(within(sheet).getByRole("button", { name: "Make the command" }));
      expect(await within(sheet).findByText("This command expired")).toBeVisible();
      expect(within(sheet).getByRole("button", { name: "Make a new command" })).toBeEnabled();
    });

    it("adds a server from a phone too, from the header", async () => {
      onPhone();
      const user = userEvent.setup();
      renderServers([web1, web2()], "/servers", {
        ...release,
        ...passkey([]),
        servers: tickets(JoinReach.ANYWHERE, [{}]).servers,
      });
      await user.click(await screen.findByRole("button", { name: "Add server" }));
      const sheet = await screen.findByRole("dialog", { name: "Add a server" });
      expect(within(sheet).getByRole("button", { name: "Make the command" })).toBeEnabled();
    });

    it("says nothing was made when the passkey doesn't confirm it", async () => {
      stubPasskeys({ get: "NotAllowedError" });
      const user = userEvent.setup();
      const api = tickets(JoinReach.ANYWHERE, [{}]);
      renderServers([web1, web2()], "/servers", { ...release, ...passkey([]), servers: api.servers });
      await user.click(await screen.findByRole("button", { name: "Add server" }));
      const sheet = await screen.findByRole("dialog", { name: "Add a server" });
      await user.click(within(sheet).getByRole("button", { name: "Make the command" }));
      expect(
        await within(sheet).findByText("Your passkey didn't confirm it, so there's no command yet. Try again."),
      ).toBeVisible();
      expect(within(sheet).getByRole("button", { name: "Make the command" })).toBeEnabled();
    });
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

  describe("labels", () => {
    const eu = aServer({ ...web1, labels: { region: "eu", pool: "web" } });
    const us = web2({ labels: { region: "us", pool: "web" } });
    const db = aServer({ id: "srv_3", name: "db-1", labels: { pool: "db" } });

    it("lists the servers a selector matches, groups their tiles by region, and keeps both in the URL", async () => {
      const user = userEvent.setup();
      const { router } = renderServers([eu, us, db], "/servers?closed=true");

      expect(await screen.findByText("3 servers in 2 regions", { selector: ".page-lead" })).toBeVisible();
      // Grouped by region first, with what has none last.
      const map = screen.getByRole("region", { name: "Servers at a glance" });
      expect(within(map).getByText("eu")).toBeVisible();
      expect(within(map).getByText("us")).toBeVisible();
      expect(within(map).getByText("No region")).toBeVisible();

      const field = screen.getByRole("combobox", { name: "Add a label to the selector" });
      await user.type(field, "pool:web{Enter}");
      await waitFor(() => expect(router.state.location.search).toEqual({ closed: true, selector: "pool:web" }));
      expect(await screen.findByText("2 match")).toBeVisible();
      const table = screen.getByRole("table", { name: "Servers matching pool:web, sorted by name" });
      expect(within(table).getAllByRole("row")).toHaveLength(3);
      expect(within(table).queryByText("db-1")).toBeNull();

      // A term it can't read says why, in the server's words, and isn't added.
      await user.type(field, "Reg!on{Enter}");
      expect(await screen.findByRole("alert")).toHaveTextContent(
        "It has !, and a label holds letters, digits, -, _ and . alone",
      );
      await user.clear(field);
      await user.type(field, "!region:eu{Enter}");
      await waitFor(() => expect(screen.getByText("1 matches")).toBeVisible());
      expect(router.state.location.search).toMatchObject({ selector: "pool:web !region:eu" });

      await user.type(field, "pool:db{Enter}");
      expect(await screen.findByText("No server matches")).toBeVisible();
      await user.click(screen.getByRole("button", { name: "Clear the selector" }));
      await waitFor(() => expect(router.state.location.search).toEqual({ closed: true }));

      // Group by another key, or none.
      await user.selectOptions(screen.getByRole("combobox", { name: "Group the servers" }), "Group by pool");
      await waitFor(() => expect(router.state.location.search).toMatchObject({ group: "pool" }));
      const byPool = screen.getByRole("region", { name: "Servers at a glance" });
      await waitFor(() => expect(within(byPool).getByText("db")).toBeVisible());
      expect(within(byPool).queryByText("eu")).toBeNull();
      await user.selectOptions(screen.getByRole("combobox", { name: "Group the servers" }), "No groups");
      await waitFor(() => expect(router.state.location.search).toMatchObject({ group: "none" }));
      const flat = screen.getByRole("region", { name: "Servers at a glance" });
      await waitFor(() => expect(within(flat).queryByText("web")).toBeNull());
    });

    it("shows no selector for a fleet without labels", async () => {
      renderServers([web1, web2()]);
      await screen.findByRole("table", { name: "Servers, sorted by name" });
      expect(screen.queryByRole("combobox", { name: "Add a label to the selector" })).toBeNull();
    });

    it("edits a server's labels from the inspector, offering what the fleet uses, and plans them", async () => {
      const user = userEvent.setup();
      const planned: PlanServerLabelsRequest[] = [];
      const host = { kind: "server.host", scope: "srv_1", name: "host" };
      const change = create(ChangeSchema, {
        id: "chg_labels",
        summary: "Label web-1 tier:db, without pool",
        state: ChangeState.PLANNED,
        proposedVia: Surface.PANEL,
        plan: {
          fingerprint: "1a2b3c4d5e6f708192a3b4c5d6e7f809",
          resources: [
            {
              id: "res_host",
              resource: host,
              action: PlanAction.UPDATE,
              servers: ["srv_1"],
              changes: [
                { path: "labels[pool]", op: FieldOp.REMOVED, from: "web" },
                { path: "labels[tier]", op: FieldOp.ADDED, to: "db" },
              ],
            },
          ],
          // As the engine plans it: a server with no step isn't among the plan's servers.
          servers: [],
          notes: [{ reason: "labels_only", names: ["web-1"], named: true, message: "Changes labels alone" }],
        },
      });
      renderServers([eu, us, db], "/servers?server=srv_1", {
        changes: {
          planServerLabels: (req: PlanServerLabelsRequest) => {
            planned.push(req);
            return { change };
          },
          getChange: () => ({ change }),
          watchChange: feed<WatchChangeResponse>().stream,
        },
      });

      const inspector = await screen.findByRole("complementary", { name: "web-1" });
      expect(inspector).toHaveTextContent("Labelspool:webregion:eu");
      await user.click(within(inspector).getByRole("button", { name: "Edit web-1's labels" }));
      const sheet = await screen.findByRole("dialog", { name: "Labels of web-1" });
      const field = within(sheet).getByRole("combobox", { name: "Labels" });
      // What the fleet uses, but what it has, is offered.
      const offered = [...(sheet.querySelectorAll("datalist option") as NodeListOf<HTMLOptionElement>)].map(
        (o) => o.value,
      );
      expect(offered).toEqual(["pool:db", "region:us", "pool:", "region:"]);
      expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();

      await user.click(within(sheet).getByRole("button", { name: "Remove pool:web" }));
      await user.type(field, "tier:db{Enter}");
      await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
      await waitFor(() => expect(planned).toHaveLength(1));
      expect(planned[0]).toMatchObject({ serverIds: ["srv_1"], set: ["tier:db"], remove: ["pool"] });
      expect(await screen.findByText("Labels alone: no step runs on web-1, online or offline.")).toBeVisible();
      // The host goes by its server's name, and its labels as the panel writes them.
      const plan = screen.getByRole("dialog", { name: "Review plan" });
      expect(plan).toHaveTextContent("web-1");
      expect(plan).toHaveTextContent("tier:db");
      expect(plan).toHaveTextContent("pool:web");
      expect(plan).not.toHaveTextContent("tier=db");
      expect(plan).not.toHaveTextContent("server.host");
      expect(plan).toHaveTextContent("web-1 · no step, nothing goes down");
    });

    it("gives a new server labels as it joins", async () => {
      stubPasskeys();
      const user = userEvent.setup();
      const made: CreateJoinTicketRequest[] = [];
      renderServers([eu, us], "/servers?closed=true", {
        auth: {
          beginStepUp: () => ({
            ceremonyId: "cer_1",
            optionsJson: JSON.stringify({ challenge: "AAAA", allowCredentials: [] }),
          }),
          finishStepUp: () => ({ proof: "proof-1" }),
        },
        servers: {
          createJoinTicket: (req: CreateJoinTicketRequest) => {
            made.push(req);
            if (!req.proof) throw authError(Code.PermissionDenied, "needs_step_up", "this needs your passkey just now");
            const labels = Object.fromEntries(req.labels.map((l) => l.split(":") as [string, string]));
            return {
              ticket: {
                id: "jtk_1",
                state: JoinTicketState.WAITING,
                labels,
                addresses: [{ url: "https://203.0.113.7:7443", reach: JoinReach.ANYWHERE }],
              },
              value: "TICKET",
            };
          },
          getJoinTicket: () => ({ ticket: { id: "jtk_1", state: JoinTicketState.WAITING } }),
        },
      });

      await user.click(await screen.findByRole("button", { name: "Add server" }));
      const sheet = await screen.findByRole("dialog", { name: "Add a server" });
      const field = within(sheet).getByRole("combobox", { name: /^Labels/ });
      await user.type(field, "region:eu{Enter}pool:web{Enter}");
      await user.click(within(sheet).getByRole("button", { name: "Make the command" }));
      await within(sheet).findByText("Waiting for the server to join");
      expect(made.at(-1)?.labels).toEqual(["pool:web", "region:eu"]);
      expect(sheet).toHaveTextContent("Joins withpool:webregion:eu");
    });
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
