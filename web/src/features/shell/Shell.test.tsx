// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  BackupSchema,
  BackupState,
  ChangeSchema,
  ChangeState,
  Connection,
  GetVersionResponseSchema,
  ListChangesResponseSchema,
  Role,
  ServerInventorySchema,
  ServerStrain,
  Surface,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { anApp, aWebsite } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, sample, snapshotComplete } from "../../test/servers";

const hz1 = aServer({
  id: "srv_1",
  name: "hz-1",
  controlServer: true,
  stats: sample(23.4),
  inventory: create(ServerInventorySchema, {
    osName: "Ubuntu 24.04.1 LTS",
    addresses: ["10.0.0.2", "49.12.33.7", "2a01:4f8:c012:9a3e::1"],
  }),
});
const hz2 = aServer({ id: "srv_2", name: "hz-2", stats: sample(8) });

/** Renders the panel at path with these servers, once their snapshot is in. */
async function renderWith(path: string, ...servers: ReturnType<typeof aServer>[]) {
  return renderWithChanges(path, [], ...servers);
}

async function renderWithChanges(
  path: string,
  changes: ReturnType<typeof waiting>[],
  ...servers: ReturnType<typeof aServer>[]
) {
  const watch = feed<WatchServersResponse>();
  watch.push(...servers.map(changed), snapshotComplete);
  const rendered = renderAt(path, {
    servers: { watchServers: watch.stream },
    changes: { listChanges: () => create(ListChangesResponseSchema, { changes }) },
  });
  await screen.findByRole("heading", { level: 1 });
  return { ...rendered, watch };
}

/** A planned change, as the CLI leaves one that only a person can apply. */
function waiting(
  id: string,
  init: Partial<{ via: Surface; state: ChangeState; destructive: boolean; hostAccess: boolean }> = {},
) {
  return create(ChangeSchema, {
    id,
    summary: "Trust 127.0.0.1 on www",
    state: init.state ?? ChangeState.PLANNED,
    proposedVia: init.via ?? Surface.CLI,
    plan: { destructive: init.destructive ?? false, hostAccess: init.hostAccess ?? true },
  });
}

const rail = () => screen.getByRole("navigation", { name: "Main" });

/** What a link shows, leaving out what screen readers skip, such as the narrow rail's tooltip. */
function shown(link: HTMLElement): string {
  const copy = link.cloneNode(true) as HTMLElement;
  for (const hidden of copy.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
  return copy.textContent ?? "";
}

describe("the shell on wide screens", () => {
  it("shows one server the way the single-server mocks do", async () => {
    await renderWith("/", hz1);
    const nav = rail();
    await within(nav).findByText("Machine");
    expect(within(nav).getByRole("link", { name: "Server hz-1" })).toHaveAttribute("href", "/servers/srv_1");
    expect(within(nav).queryByText("Fleet")).toBeNull();
    expect(within(nav).queryByRole("link", { name: "Fleet actions" })).toBeNull();
    expect(await within(nav).findByRole("link", { name: "Dockwell 0.4.0: control plane status" })).toHaveAttribute(
      "href",
      "/settings/control-plane",
    );

    const top = screen.getByRole("banner");
    expect(top).toHaveTextContent("hz-1");
    expect(top).toHaveTextContent("Healthy");
    // The public IPv4 address, not the private one listed first.
    expect(top).toHaveTextContent("49.12.33.7");
    expect(top).not.toHaveTextContent("10.0.0.2");
  });

  it("offers New in the top bar, and counts the apps beside Apps", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete);
    renderAt("/", {
      servers: { watchServers: watch.stream },
      services: {
        listServices: () => ({ services: [anApp("web", "nginx:1.27"), anApp("api", "ghcr.io/example/api:2")] }),
      },
    });
    await screen.findByRole("heading", { level: 1 });
    // New is a short menu: an app, a database, a domain, a server, each straight to its sheet (#617).
    await userEvent.setup().click(within(screen.getByRole("banner")).getByRole("button", { name: "New" }));
    const menu = within(screen.getByRole("menu", { name: "New" }));
    expect(menu.getAllByRole("menuitem").map((item) => [item.textContent, item.getAttribute("href")])).toEqual([
      ["App", "/apps/new"],
      ["Database", "/databases?new=postgres"],
      ["Domain", "/domains?add=true"],
      ["Server", "/servers?add=true"],
    ]);
    // A glance at what Apps says in full, so screen readers hear the label alone.
    const apps = within(rail()).getByRole("link", { name: "Apps" });
    await waitFor(() => expect(apps).toHaveTextContent(/^Apps\s*2/));
  });

  it("counts no app beside Apps for a website's own web service, as the Apps page lists none", async () => {
    const { web, websites } = aWebsite();
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete);
    const { unmount } = renderAt("/apps", {
      servers: { watchServers: watch.stream },
      services: { listServices: () => ({ services: [web] }) },
      websites,
    });
    // Once the page has read both, the rail says what it says.
    expect(await screen.findByRole("heading", { name: "No apps yet" })).toBeVisible();
    const apps = within(rail()).getByRole("link", { name: "Apps" });
    expect(apps.querySelector(".rail-count")).toBeNull();
    expect(shown(apps).trim()).toBe("Apps");
    unmount();

    const again = feed<WatchServersResponse>();
    again.push(changed(hz1), snapshotComplete);
    renderAt("/", {
      servers: { watchServers: again.stream },
      services: { listServices: () => ({ services: [web, anApp("api", "ghcr.io/example/api:2")] }) },
      websites,
    });
    await screen.findByRole("heading", { level: 1 });
    await waitFor(() =>
      expect(within(rail()).getByRole("link", { name: "Apps" })).toHaveTextContent(/^Apps\s*1(?!\d)/),
    );
  });

  it("brings in fleet concepts once a second server joins", async () => {
    const { watch } = await renderWith("/", hz1);
    await within(rail()).findByText("Machine");

    act(() => watch.push(changed(hz2)));

    const nav = rail();
    expect(await within(nav).findByText("Fleet")).toBeVisible();
    expect(within(nav).getByRole("link", { name: "Servers 2" })).toHaveAttribute("href", "/servers");
    expect(within(nav).getByRole("link", { name: "Fleet actions" })).toHaveAttribute("href", "/fleet-actions");
    expect(within(nav).queryByText("Machine")).toBeNull();
    // The version is a single-server detail; the command bar still reaches the status page.
    expect(within(nav).queryByRole("link", { name: /control plane status/ })).toBeNull();
    expect(screen.getByRole("banner")).toHaveTextContent("2 servers");
    expect(screen.getByRole("banner")).toHaveTextContent("All healthy");
  });

  it("says when a server in the fleet is down", async () => {
    await renderWith("/", hz1, aServer({ ...hz2, connection: Connection.UNREACHABLE }));
    expect(await within(screen.getByRole("banner")).findByText("1 down")).toBeVisible();
  });

  it("lays out every section of the design's rail, in its order", async () => {
    await renderWith("/", hz1);
    await within(rail()).findByRole("link", { name: "Dockwell 0.4.0: control plane status" });
    const names = within(rail()).getAllByRole("link").map(shown);
    expect(names).toEqual([
      "0.4.0",
      "Overview",
      "Apps",
      "Websites",
      "Databases",
      "Connections",
      "Domains",
      "Backups",
      "Variables",
      "Catalog",
      "Server hz-1",
      "Activity",
      "Alerts",
      "Scheduled tasks",
      "Settings",
      "Mira Chen Owner",
    ]);
    const you = within(rail()).getByRole("link", { name: "Mira Chen Owner" });
    expect(you).toHaveAttribute("href", "/profile");
    expect(you.querySelector(".rail-avatar")).toHaveTextContent("MC");
  });

  it("names a viewer's role under their name", async () => {
    renderAt("/", { auth: { getAuthState: () => authState({ user: { ...mira, role: Role.VIEWER } }) } });
    expect(
      await within(await screen.findByRole("navigation", { name: "Main" })).findByRole("link", {
        name: "Mira Chen Viewer",
      }),
    ).toHaveAttribute("href", "/profile");
  });

  // Someone who didn't run the CLI learns that a change waits for them (#112).
  it("counts on Activity the changes that wait for a person", async () => {
    await renderWithChanges(
      "/",
      [
        waiting("chg_1"),
        waiting("chg_2", { hostAccess: false, destructive: true }),
        // Not one: a token may apply it, a person proposed it in the panel, or it has gone on.
        waiting("chg_3", { hostAccess: false }),
        waiting("chg_4", { via: Surface.PANEL }),
        waiting("chg_5", { state: ChangeState.DISCARDED }),
      ],
      hz1,
    );
    const activity = await within(rail()).findByRole("link", { name: "Activity 2 changes need your approval" });
    expect(activity.querySelector(".rail-count")).toHaveTextContent("2");
  });

  it("names each item once, keeping the narrow rail's tooltip from screen readers", async () => {
    await renderWith("/", hz1);
    const items = rail().querySelectorAll<HTMLElement>(".rail-item");
    expect(items.length).toBeGreaterThan(10);
    for (const item of items) {
      const label = item.querySelector(".rail-label")?.textContent ?? "";
      const tip = item.querySelector(".rail-tip");
      expect(tip).toHaveTextContent(label);
      expect(tip).toHaveAttribute("aria-hidden", "true");
      expect(item).toHaveAccessibleName(shown(item).trim());
      // Generated content would join the name, as "Overview Overview".
      expect(item).not.toHaveAttribute("data-label");
    }
  });

  it("counts a degraded server in the top bar, and doesn't say all are healthy", async () => {
    await renderWith(
      "/",
      hz1,
      aServer({ ...hz2, strainedSince: timestampFromDate(new Date(2026, 10, 7, 13, 58)), strain: ServerStrain.CPU }),
    );
    const top = screen.getByRole("banner");
    expect(await within(top).findByText("1 degraded")).toBeVisible();
    expect(within(top).queryByText("All healthy")).toBeNull();
  });

  it("says the counts may be stale while the watch reconnects", async () => {
    const { watch } = await renderWith("/", hz1, hz2);
    const top = screen.getByRole("banner");
    expect(await within(top).findByText("All healthy")).toBeVisible();

    act(() => watch.fail(new Error("the stream dropped")));

    expect(await within(top).findByRole("status")).toHaveTextContent("Reconnecting");
    expect(top).not.toHaveTextContent("All healthy");
    expect(top).not.toHaveTextContent("Deploying");
    expect(top).toHaveTextContent("2 servers");
  });

  it("says a single server's state may be stale while the watch reconnects", async () => {
    const { watch } = await renderWith("/", hz1);
    const top = screen.getByRole("banner");
    expect(await within(top).findByText("Healthy")).toBeVisible();

    act(() => watch.fail(new Error("the stream dropped")));

    expect(await within(top).findByText("Reconnecting")).toBeVisible();
    expect(top).not.toHaveTextContent("Healthy");
    expect(top).not.toHaveTextContent("Deploying");
  });

  it("counts every state, and calls the fleet healthy only when every server is connected", async () => {
    await renderWith(
      "/",
      hz1,
      aServer({ id: "srv_2", name: "hz-2", connection: Connection.PENDING }),
      aServer({ id: "srv_3", name: "hz-3", connection: Connection.UNSPECIFIED }),
      aServer({ id: "srv_4", name: "hz-4", connection: Connection.UNREACHABLE }),
    );
    const top = screen.getByRole("banner");
    expect(await within(top).findByText("4 servers")).toBeVisible();
    expect(within(top).getByText("1 down")).toBeVisible();
    expect(within(top).getByText("1 waiting for an agent")).toBeVisible();
    expect(within(top).getByText("1 in an unknown state")).toBeVisible();
    expect(top).not.toHaveTextContent("All healthy");
  });

  it("doesn't call a fleet with a server in an unknown state healthy", async () => {
    await renderWith("/", hz1, aServer({ id: "srv_2", name: "hz-2", connection: Connection.UNSPECIFIED }));
    const top = screen.getByRole("banner");
    expect(await within(top).findByText("1 in an unknown state")).toBeVisible();
    expect(top).not.toHaveTextContent("All healthy");
  });

  it("marks where you are, including the pages below a section", async () => {
    const user = userEvent.setup();
    await renderWith("/", hz1);
    expect(within(rail()).getByRole("link", { name: "Overview" })).toHaveAttribute("aria-current", "page");

    await user.click(within(rail()).getByRole("link", { name: "Alerts" }));

    expect(await screen.findByRole("heading", { level: 1, name: "Alerts" })).toBeVisible();
    expect(within(rail()).getByRole("link", { name: "Alerts" })).toHaveAttribute("aria-current", "page");
    expect(within(rail()).getByRole("link", { name: "Overview" })).not.toHaveAttribute("aria-current");
    expect(document.title).toBe("Alerts · Dockwell");
  });

  it("moves focus to the new page after a move, and skips to it from the top", async () => {
    const user = userEvent.setup();
    await renderWith("/", hz1);
    const main = screen.getByRole("main");

    await user.tab();
    const skip = screen.getByRole("link", { name: "Skip to content" });
    expect(skip).toHaveFocus();
    expect(skip).toHaveAttribute("href", "#main");
    expect(main).toHaveAttribute("id", "main");

    await user.click(within(rail()).getByRole("link", { name: "Websites" }));
    await screen.findByRole("heading", { level: 1, name: "Websites" });
    await waitFor(() => expect(main).toHaveFocus());
  });

  it("reaches every link in the rail with Tab", async () => {
    const user = userEvent.setup();
    await renderWith("/", hz1);
    const links = within(rail()).getAllByRole("link");
    await user.tab(); // the skip link
    for (const link of links) {
      await user.tab();
      expect(link).toHaveFocus();
    }
  });

  it("passes axe", async () => {
    const { container } = await renderWith("/apps", hz1);
    await within(rail()).findByText("0.4.0");
    await expectAccessible(container);
  });
});

describe("the command bar", () => {
  it("opens on Ctrl+K and goes where you choose", async () => {
    const user = userEvent.setup();
    const { router } = await renderWith("/apps", hz1);

    await user.keyboard("{Control>}k{/Control}");
    const dialog = screen.getByRole("dialog", { name: "Command bar" });
    expect(within(dialog).getByRole("combobox")).toHaveFocus();
    expect(within(dialog).getByRole("option", { name: "Open hz-1 Healthy · 49.12.33.7" })).toBeVisible();

    await user.keyboard("control plane{Enter}");

    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/control-plane"));
    expect(await screen.findByRole("heading", { level: 1, name: "Control plane" })).toBeVisible();
  });

  it("opens from the search button, which teaches the shortcut, and closes on Escape", async () => {
    const user = userEvent.setup();
    await renderWith("/", hz1);
    const search = screen.getByRole("button", { name: "Search or run a command" });
    expect(search).toHaveAttribute("aria-keyshortcuts", "Control+K");
    expect(within(search).getByText("Ctrl K")).toHaveAttribute("aria-hidden", "true");

    await user.click(search);
    expect(screen.getByRole("dialog", { name: "Command bar" })).toBeVisible();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(search).toHaveFocus();
  });

  it("toggles with the shortcut, and takes ⌘K on Apple keyboards", async () => {
    vi.spyOn(Navigator.prototype, "platform", "get").mockReturnValue("MacIntel");
    const user = userEvent.setup();
    await renderWith("/", hz1);
    expect(screen.getByRole("button", { name: "Search or run a command" })).toHaveAttribute(
      "aria-keyshortcuts",
      "Meta+K",
    );

    await user.keyboard("{Control>}k{/Control}");
    expect(screen.queryByRole("dialog")).toBeNull();
    await user.keyboard("{Meta>}k{/Meta}");
    expect(screen.getByRole("dialog", { name: "Command bar" })).toBeVisible();
    await user.keyboard("{Meta>}k{/Meta}");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it.each([
    ["Russian", "л"],
    ["Greek", "κ"],
    ["Hebrew", "ל"],
  ])("opens on Ctrl+K on a %s layout, where that key types %s", async (_layout, key) => {
    await renderWith("/", hz1);
    fireEvent.keyDown(window, { key, code: "KeyK", ctrlKey: true });
    expect(screen.getByRole("dialog", { name: "Command bar" })).toBeVisible();
  });

  it("goes by the letter on Latin layouts that move K", async () => {
    await renderWith("/", hz1);
    // Dvorak types T where QWERTY has K, and K elsewhere.
    fireEvent.keyDown(window, { key: "t", code: "KeyK", ctrlKey: true });
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.keyDown(window, { key: "k", code: "KeyV", ctrlKey: true });
    expect(screen.getByRole("dialog", { name: "Command bar" })).toBeVisible();
  });

  it("toggles once per press while the keys are held", async () => {
    await renderWith("/", hz1);
    fireEvent.keyDown(window, { key: "k", code: "KeyK", ctrlKey: true });
    for (let i = 0; i < 3; i++) fireEvent.keyDown(window, { key: "k", code: "KeyK", ctrlKey: true, repeat: true });
    expect(screen.getByRole("dialog", { name: "Command bar" })).toBeVisible();
  });

  it("leaves the shortcut alone while an input method composes", async () => {
    await renderWith("/", hz1);
    fireEvent.keyDown(window, { key: "k", ctrlKey: true, isComposing: true });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("switches the theme", async () => {
    const user = userEvent.setup();
    await renderWith("/", hz1);
    await user.keyboard("{Control>}k{/Control}");
    await user.keyboard("dark theme{Enter}");
    expect(document.documentElement).toHaveAttribute("data-theme", "dark");

    await user.keyboard("{Control>}k{/Control}");
    expect(screen.getByRole("option", { name: "Use the dark theme Current" })).toBeVisible();
    await user.keyboard("system theme{Enter}");
    expect(document.documentElement).not.toHaveAttribute("data-theme");
  });

  it("lists every server in a fleet", async () => {
    const user = userEvent.setup();
    await renderWith("/", hz1, hz2);
    await user.keyboard("{Control>}k{/Control}");
    const servers = screen.getByRole("group", { name: "Servers" });
    expect(
      within(servers)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual([
      "Open hz-1 Healthy · 49.12.33.7",
      "Show what happened on hz-1",
      "Open hz-2 Healthy",
      "Show what happened on hz-2",
    ]);
    expect(screen.getByRole("option", { name: "Open the server list 2" })).toBeVisible();
    await expectAccessible(document.body);
  });

  it("says a server's address is private where it can't show a tag", async () => {
    const user = userEvent.setup();
    const natted = aServer({
      id: "srv_1",
      name: "hz-1",
      stats: sample(23.4),
      inventory: create(ServerInventorySchema, { addresses: ["10.0.0.25"] }),
    });
    await renderWith("/apps", natted);
    await user.keyboard("{Control>}k{/Control}");
    const dialog = screen.getByRole("dialog", { name: "Command bar" });
    expect(within(dialog).getByRole("option", { name: "Open hz-1 Healthy · 10.0.0.25, private" })).toBeVisible();
  });

  it("opens your profile", async () => {
    const user = userEvent.setup();
    const { router } = await renderWith("/", hz1);
    await user.keyboard("{Control>}k{/Control}");
    await user.click(screen.getByRole("option", { name: "Open your profile and security settings mira@acme.dev" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/profile"));
    expect(await screen.findByRole("heading", { level: 1, name: "Profile and security" })).toBeVisible();
  });

  it("opens a server's own page", async () => {
    const user = userEvent.setup();
    const { router } = await renderWith("/", hz1, hz2);
    await user.keyboard("{Control>}k{/Control}");
    await user.click(screen.getByRole("option", { name: "Open hz-2 Healthy" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/servers/srv_2"));
    expect(await screen.findByRole("heading", { level: 1, name: "hz-2" })).toBeVisible();
  });
});

describe("the shell on phones", () => {
  it("swaps the rail and top bar for the phone header and tab bar", async () => {
    onPhone();
    const { container } = await renderWith("/", hz1);
    expect(screen.queryByRole("banner", { name: /Search/ })).toBeNull();
    expect(screen.queryByText("Machine")).toBeNull();
    const tabs = within(screen.getByRole("navigation", { name: "Main" })).getAllByRole("link");
    expect(tabs.map((tab) => tab.textContent)).toEqual(["Overview", "Apps", "Server", "Activity", "More"]);
    expect(tabs[0]).toHaveAttribute("aria-current", "page");
    expect(screen.getAllByRole("heading", { level: 1 })).toHaveLength(1);
    await expectAccessible(container);
  });

  it("marks Activity when a change waits for a person", async () => {
    onPhone();
    await renderWithChanges("/", [waiting("chg_1")], hz1);
    const tab = await within(screen.getByRole("navigation", { name: "Main" })).findByRole("link", {
      name: "Activity 1 change needs your approval",
    });
    expect(tab.querySelector(".tabbar-dot")).not.toBeNull();
  });

  it("names the Servers tab for a fleet", async () => {
    onPhone();
    await renderWith("/", hz1, hz2);
    // The page's heading is there before the watch has brought both servers, so wait for the name.
    expect(
      await within(screen.getByRole("navigation", { name: "Main" })).findByRole("link", { name: "Servers" }),
    ).toBeVisible();
  });

  it("keeps More current in the sections it holds, with a way back", async () => {
    onPhone();
    const user = userEvent.setup();
    await renderWith("/more", hz1);
    const tabs = screen.getByRole("navigation", { name: "Main" });
    expect(within(tabs).getByRole("link", { name: "More" })).toHaveAttribute("aria-current", "page");
    expect(screen.queryByRole("link", { name: "Back" })).toBeNull();

    await user.click(screen.getByRole("link", { name: "Domains" }));

    expect(await screen.findByRole("heading", { level: 1, name: "Domains" })).toBeVisible();
    expect(within(tabs).getByRole("link", { name: "More" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/more");
  });

  it("says on More how many domains there are, and when the last backup was, as M-More does", async () => {
    onPhone();
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete);
    renderAt("/more", {
      servers: { watchServers: watch.stream },
      domains: {
        listDomains: () => ({
          domains: [
            { name: "www.example.com", environmentId: "env_prod", routes: [] },
            { name: "api.example.com", environmentId: "env_prod", routes: [] },
          ],
        }),
      },
      backups: {
        listBackups: () => ({
          backups: [
            create(BackupSchema, {
              id: "bkp_1",
              volume: "orders",
              state: BackupState.DONE,
              startedAt: timestampFromDate(new Date(Date.now() - 2 * 3_600_000)),
            }),
          ],
        }),
      },
    });
    expect(await screen.findByRole("link", { name: "Domains 2" })).toHaveAttribute("href", "/domains");
    const backupsLink = await screen.findByRole("link", { name: /^Backups/ });
    // By the control plane's clock, as every time the panel says is.
    await waitFor(() => expect(backupsLink).toHaveTextContent(/^Backups \d+ h ago$/));
  });

  it("lists what the tab bar has no room for on More, and sets the theme there", async () => {
    onPhone();
    const user = userEvent.setup();
    const { container } = await renderWith("/more", hz1);
    expect(
      within(screen.getByRole("navigation", { name: "Resources" }))
        .getAllByRole("link")
        .map((link) => link.textContent),
    ).toEqual(["Variables and secrets", "Websites", "Databases", "Connections", "Domains", "Backups", "Catalog"]);
    expect(
      within(screen.getByRole("navigation", { name: "Operate" }))
        .getAllByRole("link")
        .map((link) => link.textContent),
    ).toEqual(["Alerts", "Scheduled tasks"]);
    expect(await screen.findByRole("link", { name: "Control plane 0.4.0" })).toHaveAttribute(
      "href",
      "/settings/control-plane",
    );
    expect(screen.getByRole("link", { name: "Profile and security Mira Chen" })).toHaveAttribute("href", "/profile");
    expect(screen.getByRole("button", { name: "Sign out" })).toBeVisible();

    await user.selectOptions(screen.getByRole("combobox", { name: "Theme" }), "Dark");
    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    await expectAccessible(container);
  });

  it("opens the command bar from the header's search button, with Cancel", async () => {
    onPhone();
    const user = userEvent.setup();
    await renderWith("/", hz1);
    await user.click(screen.getByRole("button", { name: "Search or run a command" }));
    const dialog = screen.getByRole("dialog", { name: "Command bar" });
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("the shell in Simplified Chinese", () => {
  it("translates the rail, the top bar and the tab bar", async () => {
    await activateLocale("zh-CN");
    await renderWith("/", hz1);
    const nav = screen.getByRole("navigation", { name: "主导航" });
    expect(within(nav).getByRole("link", { name: "概览" })).toHaveAttribute("aria-current", "page");
    expect(within(nav).getByText("机器")).toBeVisible();
    expect(within(nav).getByRole("link", { name: "计划任务" })).toBeVisible();
    expect(screen.getByRole("button", { name: "搜索或运行命令" })).toBeVisible();
    expect(screen.getByRole("banner")).toHaveTextContent("健康");
  });

  it("counts a fleet in the top bar with the design system's words", async () => {
    await activateLocale("zh-CN");
    await renderWith("/", hz1, hz2);
    const top = screen.getByRole("banner");
    expect(await within(top).findByText("2 台服务器")).toBeVisible();
    expect(within(top).getByText("全部健康")).toBeVisible();
  });

  it("translates the not-yet page", async () => {
    await activateLocale("zh-CN");
    renderAt("/connections", {
      system: { getVersion: () => create(GetVersionResponseSchema, {}) },
      servers: { watchServers: feed<WatchServersResponse>().stream },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "连接" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 2, name: "此版本尚未提供" })).toBeVisible();
  });
});
