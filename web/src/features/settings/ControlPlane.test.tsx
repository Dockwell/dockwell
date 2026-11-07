// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  Connection,
  ControlPlaneSchema,
  GetControlPlaneResponseSchema,
  PanelServedBy,
  StateStoreEngine,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { authState } from "../../test/auth";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { copyCommand, INSTALL_COMMAND } from "./ControlPlanePage";

const MiB = 1024 * 1024;

type Plane = MessageInitShape<typeof ControlPlaneSchema>;

function plane(init: Plane = {}) {
  return create(GetControlPlaneResponseSchema, {
    controlPlane: create(ControlPlaneSchema, {
      build: { version: "v0.4.0", commit: "3f2a9c1d4e5b6a7b", commitTime: timestampFromDate(new Date(2026, 10, 3)) },
      startTime: timestampFromDate(new Date(Date.now() - 3 * 86_400_000)),
      dataDir: "/var/lib/dockwell",
      keyFile: "/var/lib/dockwell/control/key",
      store: {
        engine: StateStoreEngine.SQLITE,
        location: "/var/lib/dockwell/control/state.db",
        version: "3.50.4",
        sizeBytes: BigInt(14 * MiB),
        roundTrip: { nanos: 400_000 },
      },
      panel: { listen: "127.0.0.1:7600", servedBy: PanelServedBy.NONE },
      agents: { listen: ":7443", url: "https://192.0.2.10:7443", publicUrls: ["https://203.0.113.7:7443"] },
      memoryBytes: BigInt(96 * MiB),
      ...init,
    } as Plane),
  });
}

const control = aServer({ id: "srv_1", name: "hz-1", controlServer: true, agentBuild: { version: "v0.4.0" } });
const away = aServer({
  id: "srv_2",
  name: "hz-2",
  connection: Connection.UNREACHABLE,
  agentBuild: { version: "v0.4.0" },
  lastSeenTime: timestampFromDate(new Date(Date.now() - 12 * 60_000)),
});
const behind = aServer({ id: "srv_3", name: "pi-1", agentBuild: { version: "v0.3.2" } });

function renderPlane(init: Plane = {}, more: Api = {}, servers = [control]) {
  const watch = feed<WatchServersResponse>();
  watch.push(...servers.map(changed), snapshotComplete);
  return renderAt("/settings/control-plane", {
    servers: { watchServers: watch.stream },
    settings: { getControlPlane: () => plane(init) },
    ...more,
  });
}

/** A panel by its title, once what it says has come. */
async function panel(name: string): Promise<HTMLElement> {
  await waitFor(() => expect(screen.getByRole("region", { name }).querySelector("[aria-busy=true]")).toBeNull());
  return screen.getByRole("region", { name });
}

describe("Control plane", () => {
  it("says what runs it, where it keeps its state, and how it's doing", async () => {
    const { container } = renderPlane();
    expect(await screen.findByRole("heading", { level: 1, name: "Control plane" })).toBeVisible();
    expect(
      within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: "Control plane" }),
    ).toHaveAttribute("aria-current", "page");
    expect(await screen.findByText("Panel, API and scheduler for this install · one replica, on hz-1")).toBeVisible();

    const replica = await screen.findByRole("article", { name: "hz-1" });
    await waitFor(() => expect(replica).not.toHaveAttribute("aria-busy", "true"));
    expect(within(replica).getByText("Healthy")).toBeVisible();
    expect(within(replica).getByRole("link", { name: "hz-1" })).toHaveAttribute("href", "/servers/srv_1");
    expect(within(replica).getByText("SQLite, on this server")).toBeVisible();
    expect(within(replica).getByText("0.4.0")).toHaveAttribute("title", "v0.4.0");
    expect(within(replica).getByText("96 MB with its agent, of a 250 MB budget")).toBeVisible();

    const store = await panel("State store");
    expect(within(store).getByText("SQLite 3.50.4")).toBeVisible();
    expect(within(store).getByText("/var/lib/dockwell/control/state.db")).toBeVisible();
    expect(within(store).getByText("14 MB")).toBeVisible();
    expect(within(store).getByText("0.4 ms")).toBeVisible();
    expect(within(store).getByText("/var/lib/dockwell/control/key")).toBeVisible();

    const addresses = await panel("Addresses");
    expect(within(addresses).getByText("127.0.0.1:7600")).toBeVisible();
    expect(within(addresses).getByText(":7443")).toBeVisible();
    expect(within(addresses).getByText("https://192.0.2.10:7443")).toBeVisible();
    expect(within(addresses).getByText("https://203.0.113.7:7443")).toBeVisible();
    await expectAccessible(container);
  });

  it("names the install it serves, once it has a name", async () => {
    renderPlane({}, { auth: { getAuthState: () => authState({ installName: "Acme servers" }) } });
    expect(await screen.findByText("Panel, API and scheduler for Acme servers · one replica, on hz-1")).toBeVisible();
  });

  it("says how to back it up by hand, with this install's paths, and the key apart", async () => {
    renderPlane();
    const backups = await panel("Backups");
    expect(within(backups).getByText("By hand, for now")).toBeVisible();
    expect(
      within(backups).getByText(
        copyCommand("/var/lib/dockwell", "/var/lib/dockwell/control/key").replaceAll("\n", " "),
      ),
    ).toBeVisible();
    expect(within(backups).getByRole("link", { name: "How to back it up and restore it" })).toHaveAttribute(
      "href",
      "https://github.com/Dockwell/dockwell/blob/main/docs/install.md#back-up-the-control-plane",
    );
    expect(within(backups).getByRole("link", { name: "Backups" })).toHaveAttribute("href", "/backups");
    expect(within(backups).queryByText(/pg_dump/)).toBeNull();
  });

  it("says to dump a Postgres store as any Postgres database", async () => {
    renderPlane({
      store: {
        engine: StateStoreEngine.POSTGRES,
        location: "postgres://dockwell@db.example.com:5432/dockwell",
        version: "17.2",
        sizeBytes: BigInt(40 * MiB),
        roundTrip: { nanos: 30_000 },
      },
    });
    const store = await panel("State store");
    expect(within(store).getByText("PostgreSQL 17.2")).toBeVisible();
    expect(within(store).getByText("under 0.1 ms")).toBeVisible();
    expect(within(store).getByText("Ready for more than one control plane")).toBeVisible();
    expect(within(await panel("Backups")).getByText("pg_dump")).toBeVisible();
    expect(within(screen.getByRole("article", { name: "hz-1" })).getByText("PostgreSQL")).toBeVisible();
  });

  it("says how to upgrade, and claims nothing of releases it hasn't checked", async () => {
    renderPlane();
    const updates = await panel("Updates");
    expect(within(updates).getByText("0.4.0 runs here")).toBeVisible();
    expect(within(updates).getByText("Built 3 Nov from 3f2a9c1d4e5b")).toBeVisible();
    expect(within(updates).getByText(INSTALL_COMMAND)).toBeVisible();
    expect(INSTALL_COMMAND).toBe(
      "curl -fsSL https://raw.githubusercontent.com/Dockwell/dockwell/main/scripts/install.sh | sudo sh",
    );
    expect(within(updates).getByText("sudo sh -s -- --version 0.4.0")).toBeVisible();
    expect(within(updates).queryByText(/current|latest/i)).toBeNull();
  });

  it("counts its agents, and names those away or on another version", async () => {
    renderPlane({}, {}, [control, away, behind]);
    const agents = await panel("Agents");
    expect(within(agents).getByText("3")).toBeVisible();
    expect(within(agents).getByText("agents · 2 on 0.4.0, 1 on another version")).toBeVisible();
    expect(within(agents).getByText("2 connected")).toBeVisible();
    expect(within(agents).getByText("hz-2")).toBeVisible();
    expect(within(agents).getByText("not connected, last heard 12 min ago")).toBeVisible();
    expect(within(agents).getByText("pi-1")).toBeVisible();
    expect(within(agents).getByText("runs 0.3.2")).toBeVisible();
    expect(within(agents).getByRole("link", { name: "Servers" })).toHaveAttribute("href", "/servers");
  });

  it("says when it can't say how the control plane is, and tries again", async () => {
    const user = userEvent.setup();
    let fail = true;
    renderAt("/settings/control-plane", {
      settings: {
        getControlPlane: () => {
          if (fail) throw new ConnectError("the state store at state.db doesn't answer", Code.Unavailable);
          return plane();
        },
      },
    });
    expect(await screen.findByText("Dockwell can't say how its control plane is")).toBeVisible();
    fail = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("SQLite 3.50.4")).toBeVisible();
    expect(screen.queryByText("Dockwell can't say how its control plane is")).toBeNull();
  });

  it("is a section of Settings on a phone, which leads back to the list", async () => {
    onPhone();
    renderPlane();
    expect(await screen.findByRole("heading", { level: 1, name: "Control plane" })).toBeVisible();
    const back = screen.getByRole("link", { name: "Back" });
    expect(back).toHaveAttribute("href", "/settings");
    // Settings is above this page, not this page.
    expect(back).not.toHaveAttribute("aria-current");
    expect(screen.queryByRole("navigation", { name: "Settings" })).toBeNull();
  });
});

describe("copyCommand", () => {
  it("stops Dockwell, packs its folders with the key apart, and starts it whatever the copy did, a line each", () => {
    expect(copyCommand("/var/lib/dockwell", "/var/lib/dockwell/control/key")).toBe(
      "sudo systemctl stop dockwell\n" +
        "sudo tar -C /var/lib/dockwell --exclude control/key -czf /root/dockwell-control.tgz control agent\n" +
        "sudo cp -p /var/lib/dockwell/control/key /root/dockwell-key\n" +
        "sudo systemctl start dockwell",
    );
  });
});
