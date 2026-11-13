// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  CatalogNeedOptionSchema,
  ChangeSchema,
  ChangeState,
  Engine,
  EntrySchema,
  InputType,
  type InstallCatalogEntryRequest,
  NeedSchema,
  Part,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

// WordPress as ADR 0084 has it: its database a need its own MariaDB meets, or a
// new MySQL from the catalog in its place.
const wordpress = create(EntrySchema, {
  id: "wordpress",
  name: "WordPress",
  summary: "Websites and blogs, with themes and plugins",
  category: "cms",
  version: "7.1.3",
  appLicense: "GPL-2.0-or-later",
  web: { service: "wordpress" },
  databases: ["db"],
  inputs: [
    { name: "PORT", label: "Port on this server", type: InputType.PORT, defaultValue: "8085" },
    { name: "MARIADB_ROOT_PASSWORD", label: "Database root password", type: InputType.SECRET, generate: 32 },
  ],
  needs: [
    create(NeedSchema, {
      name: "db",
      label: "Database",
      service: "db",
      database: "wordpress",
      user: "wordpress",
      accepts: [
        { engine: Engine.MYSQL, versions: ">=8.0" },
        { engine: Engine.MARIADB, versions: ">=10.6" },
      ],
      sets: [
        { name: "DB_ADDRESS", part: Part.ADDRESS },
        { name: "MARIADB_PASSWORD", part: Part.PASSWORD },
      ],
    }),
  ],
});

const ownMariaDB = create(CatalogNeedOptionSchema, {
  need: "db",
  choice: "new:mariadb",
  engine: Engine.MARIADB,
  version: "12.3.3",
});
const catalogMySQL = create(CatalogNeedOptionSchema, {
  need: "db",
  choice: "new:mysql",
  engine: Engine.MYSQL,
  version: "8.4.11",
  entry: "mysql",
  entryName: "MySQL",
});

function api(needs = [ownMariaDB, catalogMySQL], more: Api["catalog"] = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: "srv_1", name: "hz-1" })), snapshotComplete);
  return {
    servers: { watchServers: watch.stream },
    containers: { listContainers: () => ({ containers: [] }) },
    catalog: {
      listCatalogEntries: () => ({ entries: [wordpress] }),
      getCatalogEntry: () => ({
        entry: wordpress,
        project: "wordpress",
        values: { PORT: "8085" },
        parts: [
          { service: "wordpress", image: "wordpress:7.1.3-apache", opens: true },
          { service: "db", image: "mariadb:12.3.3", database: true, volumes: ["data"] },
        ],
        needs,
      }),
      ...more,
    },
  };
}

describe("An app's needs as it's installed", () => {
  it("meets each with a new one beside the app by default, a line with Change, and plans it", async () => {
    const user = userEvent.setup();
    let asked: InstallCatalogEntryRequest | undefined;
    const change = create(ChangeSchema, { id: "chg_wp", summary: "Install WordPress", state: ChangeState.PLANNED });
    const { container } = renderAt(
      "/catalog/wordpress/install",
      api(undefined, {
        installCatalogEntry: (req) => {
          asked = req;
          return { change };
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Database" });
    expect(row).toHaveTextContent("New MariaDB 12.3.3 beside WordPress");
    expect(within(row).getByRole("button", { name: "Change database" })).toBeVisible();
    await expectAccessible(container);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.needs).toEqual({ db: "new:mariadb" });
  });

  it("picks another engine the app takes, with what it takes beneath, in the plan and its CLI command", async () => {
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    let asked: InstallCatalogEntryRequest | undefined;
    const change = create(ChangeSchema, { id: "chg_wp", summary: "Install WordPress", state: ChangeState.PLANNED });
    const { container } = renderAt(
      "/catalog/wordpress/install",
      api(undefined, {
        installCatalogEntry: (req) => {
          asked = req;
          return { change };
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    await user.click(await within(sheet).findByRole("button", { name: "Change database" }));
    const picker = within(sheet).getByRole("group", { name: "Database" });
    expect(within(picker).getByRole("radio", { name: "New MariaDB 12.3.3" })).toBeChecked();
    expect(picker).toHaveTextContent("MySQL ≥8.0 · MariaDB ≥10.6");
    await user.click(within(picker).getByRole("radio", { name: "New MySQL 8.4.11" }));
    await expectAccessible(container);
    await user.click(within(sheet).getByRole("button", { name: "Advanced" }));
    await user.click(within(sheet).getByRole("button", { name: "Copy as CLI command" }));
    expect(writeText).toHaveBeenLastCalledWith("dockwell catalog install wordpress --needs db=new:mysql");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.needs).toEqual({ db: "new:mysql" });
  });

  it("offers no choice where there's one way, and asks nothing of it", async () => {
    renderAt("/catalog/wordpress/install", api([ownMariaDB]));
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Database" });
    expect(row).toHaveTextContent("New MariaDB 12.3.3 beside WordPress");
    expect(within(row).queryByRole("button")).not.toBeInTheDocument();
  });

  it("waits, and says so, where nothing here can make what the app needs, with where the catalog has one", async () => {
    const mysql = create(EntrySchema, {
      id: "mysql",
      name: "MySQL",
      summary: "Relational database",
      category: "databases",
    });
    renderAt("/catalog/wordpress/install", api([], { listCatalogEntries: () => ({ entries: [wordpress, mysql] }) }));
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Database" });
    expect(row).toHaveTextContent("Nothing here can make one yet");
    expect(within(sheet).getByText("Nothing here can make its database yet")).toBeVisible();
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    // The catalog has MySQL, which the app takes, and no MariaDB.
    const link = await within(row).findByRole("link", { name: "MySQL in the catalog" });
    expect(link).toHaveAttribute("href", "/catalog/mysql");
    expect(within(row).queryByRole("link", { name: "MariaDB in the catalog" })).toBeNull();
  });

  it("says a new one with no version without a gap where the version would be", async () => {
    const unversioned = create(CatalogNeedOptionSchema, { need: "db", choice: "new:mariadb", engine: Engine.MARIADB });
    renderAt("/catalog/wordpress/install", api([unversioned]));
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Database" });
    expect(row.textContent).toContain("New MariaDB beside WordPress");
    expect(row.textContent).not.toMatch(/MariaDB {2}beside/);
  });

  it("keeps its picker to the sheet's width on a phone", async () => {
    onPhone();
    const user = userEvent.setup();
    renderAt("/catalog/wordpress/install", api());
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    await user.click(await within(sheet).findByRole("button", { name: "Change database" }));
    const picker = within(sheet).getByRole("group", { name: "Database" });
    expect(within(picker).getAllByRole("radio")).toHaveLength(2);
    expect(picker.closest(".catalog-sheet")).toHaveClass("is-phone");
  });
});

const sharedDB = create(CatalogNeedOptionSchema, {
  need: "db",
  choice: "service:data/production/db",
  engine: Engine.MARIADB,
  version: "11.4.4",
  project: "data",
  environment: "production",
  service: "db",
  serviceId: "svc_db",
  sharedWith: ["shop"],
});
const administered = create(CatalogNeedOptionSchema, {
  need: "db",
  choice: "service:cloud/production/db",
  engine: Engine.MARIADB,
  version: "11.4.4",
  project: "cloud",
  environment: "production",
  service: "db",
  why: "nextcloud signs in to it as its administrator",
});

describe("A server you run, shared because you pick it", () => {
  it("is listed after the new ones, greyed with why where it can't be shared, and picked into the plan", async () => {
    const user = userEvent.setup();
    let asked: InstallCatalogEntryRequest | undefined;
    const change = create(ChangeSchema, { id: "chg_wp", summary: "Install WordPress", state: ChangeState.PLANNED });
    const { container } = renderAt(
      "/catalog/wordpress/install",
      api([ownMariaDB, catalogMySQL, sharedDB, administered], {
        installCatalogEntry: (req) => {
          asked = req;
          return { change };
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Database" });
    expect(row).toHaveTextContent("New MariaDB 12.3.3 beside WordPress");
    await user.click(within(row).getByRole("button", { name: "Change database" }));
    const picker = within(sheet).getByRole("group", { name: "Database" });
    expect(picker).toHaveTextContent("Use one you have");
    expect(picker).toHaveTextContent("Shared with shop · each app has a user of its own");
    const off = within(picker).getByRole("radio", { name: /db in cloud/ });
    expect(off).toBeDisabled();
    expect(picker).toHaveTextContent("nextcloud signs in to it as its administrator");
    await user.click(within(picker).getByRole("radio", { name: /db in data/ }));
    await expectAccessible(container);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.needs).toEqual({ db: "service:data/production/db" });
  });

  it("asks you to choose one where nothing new can be made", async () => {
    renderAt("/catalog/wordpress/install", api([sharedDB]));
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const picker = await within(sheet).findByRole("group", { name: "Database" });
    expect(within(picker).getByRole("radio", { name: /db in data/ })).not.toBeChecked();
    expect(within(sheet).getByText("Choose its database to review the plan")).toBeVisible();
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
  });
});
