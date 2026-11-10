// SPDX-License-Identifier: AGPL-3.0-only

import type { Server, WatchServersResponse } from "@dockwell/api";
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { feed, renderAt } from "../../test/render";
import { aServer, changed, sample, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1", stats: sample(10) });
const hz2 = aServer({ id: "srv_2", name: "hz-2", stats: sample(20) });

/**
 * The page's own box under the shell, which carries its kind, with these servers in the fleet, once what shows only
 * when their state has arrived (ready) is on screen.
 */
async function pageAt(path: string, heading: string, servers: Server[] = [hz1], ready?: string): Promise<Element> {
  const watch = feed<WatchServersResponse>();
  watch.push(...servers.map(changed), snapshotComplete);
  renderAt(path, { servers: { watchServers: watch.stream } });
  if (ready) await screen.findAllByText(ready);
  const title = await screen.findByRole("heading", { level: 1, name: heading });
  const page = title.closest(".page");
  if (!page) throw new Error(`${path} has no page around its heading`);
  return page;
}

const kindOf = (page: Element) => page.className.match(/\bis-(wide|narrow)\b/)?.[1] ?? "standard";

// How wide each grows is the stylesheet's (Page.css), and web/e2e/layout-flow.ts measures it in a browser.
describe("a page's width by its kind", () => {
  it.each([
    // Pages that use the width: a grid, one server's dashboard, a server's pages.
    ["/catalog", "Catalog", "wide"],
    ["/", "Overview", "wide", "CPU"],
    ["/servers/srv_1", "hz-1", "wide"],
    ["/servers/srv_1/files", "hz-1", "wide"],
    ["/settings/general", "General", "narrow"],
    ["/profile", "Profile and security", "narrow"],
    // A table of few columns reads at a detail page's width, its head and table sharing their edges.
    ["/apps", "Apps", "standard"],
    ["/websites", "Websites", "standard"],
    ["/domains", "Domains", "standard"],
    ["/backups", "Backups", "standard"],
    ["/activity", "Activity", "standard"],
    ["/servers", "Servers", "standard"],
    ["/more", "More", "standard"],
  ])("%s is %s", async (path, heading, kind, ready?: string) => {
    expect(kindOf(await pageAt(path, heading, [hz1], ready))).toBe(kind);
  });

  it("makes a fleet's Overview a wide page, a dashboard as one server's is", async () => {
    expect(kindOf(await pageAt("/", "Overview", [hz1, hz2], "Servers serving"))).toBe("wide");
  });
});
