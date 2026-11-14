// SPDX-License-Identifier: AGPL-3.0-only

import type { WatchServersResponse } from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const web1 = aServer({ id: "srv_1", name: "web-1", controlServer: true });
const entries = ["adminer", "gitea", "postgres", "wordpress", "uptime-kuma"].map((id) => ({
  id,
  name: id[0]?.toUpperCase() + id.slice(1),
  icon: "",
}));

function renderStart(path = "/start") {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(web1), snapshotComplete);
  return renderAt(path, {
    servers: { watchServers: watch.stream },
    catalog: { listCatalogEntries: () => ({ entries }) },
    services: { listServices: () => ({ services: [] }) },
  });
}

describe("where a newcomer starts", () => {
  it("asks what the first app comes from, each choice opening its create flow", async () => {
    const { container } = renderStart();
    expect(await screen.findByRole("heading", { level: 1, name: "What do you want to run?" })).toBeVisible();
    expect(await screen.findByText("web-1 is ready. Pick what your first app comes from.")).toBeVisible();
    const choices = screen.getByRole("list", { name: "Ways to run your first app" });
    const link = (name: string) => within(choices).getByRole("link", { name });
    expect(link("From the catalog")).toHaveAttribute("href", "/catalog");
    expect(link("A Git repository")).toHaveAttribute("href", "/apps/new?source=git");
    expect(link("A Docker image")).toHaveAttribute("href", "/apps/new?source=image");
    expect(link("A Compose file")).toHaveAttribute("href", "/apps/new?source=compose");
    expect(link("A website")).toHaveAttribute("href", "/websites/new");
    // The catalog's choice names what's in it: those a newcomer may know first, then the rest while there's room,
    // each straight into its install sheet. Four ways beside it leave room for all five, so none is "more".
    expect(await within(choices).findByRole("link", { name: "Install Adminer" })).toHaveAttribute(
      "href",
      "/apps/new?entry=adminer",
    );
    expect(within(choices).queryByText(/more$/)).toBeNull();
    expect(link("Install Postgres")).toHaveAttribute("href", "/apps/new?entry=postgres");
    expect(link("Install Wordpress")).toHaveAttribute("href", "/apps/new?entry=wordpress");
    expect(screen.getByRole("link", { name: "Skip to the overview" })).toHaveAttribute("href", "/");
    await expectAccessible(container);
  });

  it("takes a newcomer to the create flow they picked", async () => {
    const user = userEvent.setup();
    const { router } = renderStart();
    await user.click(await screen.findByRole("link", { name: "A Compose file" }));
    expect(router.state.location.pathname).toBe("/apps/new");
    expect(router.state.location.search).toEqual({ source: "compose" });
  });

  it("offers the same choices where the overview has no apps yet", async () => {
    renderStart("/");
    const choices = await screen.findByRole("list", { name: "Ways to run your first app" });
    expect(within(choices).getByRole("link", { name: "From the catalog" })).toHaveAttribute("href", "/catalog");
    expect(within(choices).getAllByRole("link")).toHaveLength(5);
  });
});
