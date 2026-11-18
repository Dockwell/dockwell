// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { EntrySchema, InputType, ServerInventorySchema, type WatchServersResponse } from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

// An app its catalog says runs on amd64 alone (ADR 0102).
const amdOnly = create(EntrySchema, {
  id: "amd-app",
  name: "Amd App",
  summary: "Runs on amd64 alone",
  category: "productivity",
  version: "1.0.0",
  appLicense: "MIT",
  architectures: ["amd64"],
  inputs: [{ name: "PASSWORD", label: "Password", type: InputType.SECRET, generate: 32 }],
});

function withServers(...servers: { id: string; name: string; architecture: string }[]): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(
    ...servers.map((s) =>
      changed(
        aServer({
          id: s.id,
          name: s.name,
          inventory: create(ServerInventorySchema, { osName: "Ubuntu 24.04.1 LTS", architecture: s.architecture }),
        }),
      ),
    ),
    snapshotComplete,
  );
  return {
    servers: { watchServers: watch.stream },
    catalog: {
      listCatalogEntries: () => ({ entries: [amdOnly] }),
      getCatalogEntry: () => ({ entry: amdOnly, project: "amd-app", values: {}, parts: [] }),
    },
  };
}

describe("An app that runs on one architecture", () => {
  it("can be put only on a server of it, and says why another can't be chosen", async () => {
    const { container } = renderAt(
      "/catalog/amd-app/install",
      withServers(
        { id: "srv_1", name: "hz-1", architecture: "amd64" },
        { id: "srv_2", name: "pi-1", architecture: "arm64" },
        { id: "srv_3", name: "edge-2", architecture: "" },
      ),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install Amd App" });
    const runsOn = await within(sheet).findByRole("group", { name: "Runs on" });
    // The one server that can take it is chosen at once, in one line (#792).
    expect(await within(runsOn).findByText("the only server that can take it")).toBeVisible();
    await userEvent.setup().click(within(runsOn).getByRole("button", { name: "Change where it runs" }));
    expect(within(runsOn).getByRole("checkbox", { name: /hz-1/ })).toBeEnabled();
    expect(within(runsOn).getByRole("checkbox", { name: /pi-1/ })).toBeDisabled();
    expect(within(runsOn).getByRole("checkbox", { name: /edge-2/ })).toBeDisabled();
    expect(runsOn).toHaveTextContent("pi-1arm64");
    expect(runsOn).toHaveTextContent("edge-2Architecture not known yet");
    expect(runsOn).toHaveTextContent("Runs on amd64 alone.");
    await expectAccessible(container);
  });

  it("says the only server can't take it, and plans nothing", async () => {
    renderAt("/catalog/amd-app/install", withServers({ id: "srv_1", name: "pi-1", architecture: "arm64" }));
    const sheet = await screen.findByRole("dialog", { name: "Install Amd App" });
    const runsOn = await within(sheet).findByRole("group", { name: "Runs on" });
    expect(within(runsOn).getByRole("alert")).toHaveTextContent("This app runs on amd64 alone, and pi-1 is arm64.");
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    // Adding a server is the way on, from here (#771).
    await userEvent.setup().click(within(runsOn).getByRole("button", { name: "Add server" }));
    expect(await screen.findByRole("dialog", { name: "Add a server" })).toBeVisible();
  });

  it("says when no server of a fleet can take it, and plans nothing", async () => {
    renderAt(
      "/catalog/amd-app/install",
      withServers(
        { id: "srv_1", name: "pi-1", architecture: "arm64" },
        { id: "srv_2", name: "pi-2", architecture: "arm64" },
      ),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install Amd App" });
    const runsOn = await within(sheet).findByRole("group", { name: "Runs on" });
    expect(within(runsOn).getByRole("checkbox", { name: /pi-1/ })).toBeDisabled();
    expect(within(runsOn).getByRole("checkbox", { name: /pi-2/ })).toBeDisabled();
    expect(within(runsOn).getByRole("alert")).toHaveTextContent(
      "Runs on amd64 alone, and none of your servers is amd64.",
    );
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    await userEvent.setup().click(within(runsOn).getByRole("button", { name: "Add server" }));
    expect(await screen.findByRole("dialog", { name: "Add a server" })).toBeVisible();
  });
});
