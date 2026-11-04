// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Connection, GetVersionResponseSchema, ServerInventorySchema, type WatchServersResponse } from "@dockwell/api";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
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
  const watch = feed<WatchServersResponse>();
  watch.push(...servers.map(changed), snapshotComplete);
  const rendered = renderAt(path, { servers: { watchServers: watch.stream } });
  await screen.findByRole("heading", { level: 1 });
  return { ...rendered, watch };
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
    expect(within(nav).getByRole("link", { name: "Server hz-1" })).toHaveAttribute("href", "/servers");
    expect(within(nav).queryByText("Fleet")).toBeNull();
    expect(within(nav).queryByRole("link", { name: "Fleet actions" })).toBeNull();
    expect(await within(nav).findByRole("link", { name: "Dockwell 0.4.0: control plane status" })).toHaveAttribute(
      "href",
      "/status",
    );

    const top = screen.getByRole("banner");
    expect(top).toHaveTextContent("hz-1");
    expect(top).toHaveTextContent("Healthy");
    // The public IPv4 address, not the private one listed first.
    expect(top).toHaveTextContent("49.12.33.7");
    expect(top).not.toHaveTextContent("10.0.0.2");
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
      "Catalog",
      "Server hz-1",
      "Activity",
      "Alerts",
      "Scheduled tasks",
      "Settings",
    ]);
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

    await waitFor(() => expect(router.state.location.pathname).toBe("/status"));
    expect(await screen.findByRole("heading", { level: 1, name: "The control plane is running" })).toBeVisible();
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
    ).toEqual(["Open hz-1 Healthy · 49.12.33.7", "Open hz-2 Healthy"]);
    expect(screen.getByRole("option", { name: "Open the server list 2" })).toBeVisible();
    await expectAccessible(document.body);
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

  it("names the Servers tab for a fleet", async () => {
    onPhone();
    await renderWith("/", hz1, hz2);
    expect(
      within(screen.getByRole("navigation", { name: "Main" })).getByRole("link", { name: "Servers" }),
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

  it("lists what the tab bar has no room for on More, and sets the theme there", async () => {
    onPhone();
    const user = userEvent.setup();
    const { container } = await renderWith("/more", hz1);
    expect(
      within(screen.getByRole("navigation", { name: "Resources" }))
        .getAllByRole("link")
        .map((link) => link.textContent),
    ).toEqual(["Websites", "Databases", "Connections", "Domains", "Backups", "Catalog"]);
    expect(
      within(screen.getByRole("navigation", { name: "Operate" }))
        .getAllByRole("link")
        .map((link) => link.textContent),
    ).toEqual(["Alerts", "Scheduled tasks"]);
    expect(await screen.findByRole("link", { name: "Control plane 0.4.0" })).toHaveAttribute("href", "/status");

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

  it("translates the not-yet page", async () => {
    await activateLocale("zh-CN");
    renderAt("/catalog", {
      system: { getVersion: () => create(GetVersionResponseSchema, {}) },
      servers: { watchServers: feed<WatchServersResponse>().stream },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "应用目录" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 2, name: "此版本尚未提供" })).toBeVisible();
  });
});
