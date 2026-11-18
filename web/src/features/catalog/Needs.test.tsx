// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  CatalogNeedOptionSchema,
  ChangeSchema,
  ChangeState,
  CloudflareName,
  Engine,
  EntrySchema,
  Feature,
  InputType,
  type InstallCatalogEntryRequest,
  Kind,
  NeedSchema,
  Part,
  RouteWay,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { keepsApart } from "./needs";

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

  it("says a shared Redis's apps can read and change each other's data, where the need sets no prefix", async () => {
    const sharedRedis = create(CatalogNeedOptionSchema, {
      ...sharedDB,
      choice: "service:data/production/cache",
      engine: Engine.REDIS,
      version: "8.2.1",
      service: "cache",
      serviceId: "svc_cache",
      sharedWith: ["shop"],
    });
    renderAt("/catalog/wordpress/install", api([sharedRedis]));
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const picker = await within(sheet).findByRole("group", { name: "Database" });
    expect(picker).toHaveTextContent("Shared with shop · its apps can read and change each other's data");
  });

  it("names the upgrade that lets another app's server be shared, plans it over the sheet, and offers the server once it's applied", async () => {
    const user = userEvent.setup();
    const kept = create(CatalogNeedOptionSchema, {
      need: "db",
      choice: "service:site/production/db",
      engine: Engine.MYSQL,
      version: "8.4.11",
      project: "site",
      environment: "production",
      service: "db",
      serviceId: "svc_site_db",
      why: "Ghost's release 4 keeps it in its own install",
      upgradeInstall: "prj_site",
      upgradeRelease: 5,
      upgradeApp: "Ghost",
    });
    const offered = create(CatalogNeedOptionSchema, {
      ...kept,
      why: "",
      upgradeInstall: "",
      upgradeRelease: 0,
      upgradeApp: "",
    });
    const needs = [ownMariaDB, catalogMySQL, kept];
    let upgraded = "";
    const change = create(ChangeSchema, {
      id: "chg_ghost",
      summary: "Upgrade Ghost to 6.72.0",
      state: ChangeState.PLANNED,
    });
    const { container } = renderAt(
      "/catalog/wordpress/install",
      api(needs, {
        upgradeCatalogInstall: (req) => {
          upgraded = req.id;
          // Once it's upgraded, the server reads as one that can be shared.
          needs[2] = offered;
          return { change };
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Database" });
    await user.click(within(row).getByRole("button", { name: "Change database" }));
    const picker = within(sheet).getByRole("group", { name: "Database" });
    expect(picker).toHaveTextContent("Ghost's release 4 keeps it in its own install");
    expect(within(picker).queryByRole("radio", { name: /db in site/ })).toBeNull();
    await expectAccessible(container);
    await user.click(within(picker).getByRole("button", { name: "Upgrade Ghost" }));
    await waitFor(() => expect(upgraded).toBe("prj_site"));
    expect(await screen.findByText("Upgrade Ghost to 6.72.0")).toBeVisible();
    // Back from the plan, the sheet asks again, and the server is offered.
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    const again = await within(sheet).findByRole("radio", { name: /db in site/ });
    expect(again).toBeEnabled();
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

describe("How apps sharing a server keep apart", () => {
  it("is a database each on SQL engines, and on Redis a key prefix only where every user there keeps to one", () => {
    const option = (engine: Engine, apart: boolean) =>
      create(CatalogNeedOptionSchema, {
        engine,
        keepsApart: apart,
        service: "cache",
        choice: "service:data/production/cache",
      });
    expect(keepsApart(option(Engine.POSTGRESQL, false))).toBe("databases");
    expect(keepsApart(option(Engine.REDIS, false))).toBe("no");
    expect(keepsApart(option(Engine.VALKEY, true))).toBe("prefixes");
  });
});

describe("Sharing a Redis whose apps keep apart without a prefix", () => {
  it("says whose data it would reach, in place of how its apps keep apart", async () => {
    const ending = create(CatalogNeedOptionSchema, {
      ...sharedDB,
      choice: "service:data/production/cache",
      engine: Engine.REDIS,
      version: "8.2.1",
      service: "cache",
      serviceId: "svc_cache",
      sharedWith: ["shop"],
      keepsApart: false,
      endsApart: ["shop"],
    });
    renderAt("/catalog/wordpress/install", api([ending]));
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const picker = await within(sheet).findByRole("group", { name: "Database" });
    expect(picker).toHaveTextContent("this one keeps no prefix: shop and it could read and change each other's data");
  });
});

// WordPress given optional storage, as ADR 0099 writes it: S3 that takes signed URLs.
const withStorage = create(EntrySchema, {
  ...wordpress,
  needs: [
    ...wordpress.needs,
    create(NeedSchema, {
      name: "storage",
      label: "Storage",
      kind: Kind.S3,
      optional: true,
      accepts: [{ kind: Kind.S3, features: [Feature.PRESIGNED_URLS] }],
      sets: [
        { name: "S3_BUCKET", part: Part.BUCKET },
        { name: "S3_SECRET_ACCESS_KEY", part: Part.SECRET_KEY },
      ],
    }),
  ],
});
const onItsOwnDisk = create(CatalogNeedOptionSchema, { need: "storage", choice: "unset" });
const uploads = create(CatalogNeedOptionSchema, {
  need: "storage",
  choice: "connection:uploads",
  connection: "uploads",
  connectionLabel: "Uploads",
});
const busy = create(CatalogNeedOptionSchema, {
  need: "storage",
  choice: "connection:media",
  connection: "media",
  connectionLabel: "Media",
  why: "it serves another app already, and isn't shared",
});

function storageApi(options = [onItsOwnDisk, uploads, busy], more: Api["catalog"] = {}): Api {
  const base = api([ownMariaDB, ...options], more);
  return {
    ...base,
    catalog: {
      ...base.catalog,
      getCatalogEntry: () => ({
        entry: withStorage,
        project: "wordpress",
        values: { PORT: "8085" },
        parts: [{ service: "wordpress", image: "wordpress:7.1.3-apache", opens: true }],
        needs: [ownMariaDB, ...options],
      }),
      ...more,
    },
    connections: { listConnections: () => ({ connections: [] }) },
  };
}

describe("An app's storage as it's installed (ADR 0099)", () => {
  it("runs on its own disk by default, and takes a connection the new project may use, one that can't off with why", async () => {
    const user = userEvent.setup();
    let asked: InstallCatalogEntryRequest | undefined;
    const change = create(ChangeSchema, { id: "chg_wp", summary: "Install WordPress", state: ChangeState.PLANNED });
    const { container } = renderAt(
      "/catalog/wordpress/install",
      storageApi(undefined, {
        installCatalogEntry: (req) => {
          asked = req;
          return { change };
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Storage" });
    expect(row).toHaveTextContent("On its own disk");
    await user.click(within(row).getByRole("button", { name: "Use a connection" }));
    const picker = within(sheet).getByRole("group", { name: "Storage" });
    expect(picker).toHaveTextContent("Needs signed URLs");
    expect(within(picker).getByRole("radio", { name: /On its own disk/ })).toBeChecked();
    expect(within(picker).getByRole("radio", { name: /Media/ })).toBeDisabled();
    // A line, with the whole reason a step away (slice 2 review, L3).
    expect(picker).not.toHaveTextContent("isn't shared");
    await user.click(within(picker).getByRole("button", { name: "Why" }));
    expect(picker).toHaveTextContent("It serves another app already, and isn't shared");
    await expectAccessible(container);
    await user.click(within(picker).getByRole("radio", { name: /Uploads/ }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.needs).toEqual({ db: "new:mariadb", storage: "connection:uploads" });
  });

  // ADR 0099: a shared connection another app uses is a way, and says it's shared.
  it("offers a shared connection another app uses, and says it's shared", async () => {
    const user = userEvent.setup();
    const shared = create(CatalogNeedOptionSchema, { ...busy, why: "", connectionShared: true });
    renderAt("/catalog/wordpress/install", storageApi([onItsOwnDisk, uploads, shared]));
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    await user.click(await within(sheet).findByRole("button", { name: "Use a connection" }));
    const picker = within(sheet).getByRole("group", { name: "Storage" });
    const media = within(picker).getByRole("radio", { name: /Media/ });
    expect(media).toBeEnabled();
    expect(media.closest("label")).toHaveTextContent("Shared with other apps, each under its own prefix");
  });

  it("takes the only connection where the app can't run without storage", async () => {
    const required = create(CatalogNeedOptionSchema, { ...uploads });
    renderAt("/catalog/wordpress/install", storageApi([required]));
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Storage" });
    expect(row).toHaveTextContent("Uploads");
    expect(within(row).getByRole("button", { name: "Change storage" })).toBeVisible();
  });

  it("says no connection meets it yet where none can, and adds one from the sheet", async () => {
    const user = userEvent.setup();
    renderAt("/catalog/wordpress/install", storageApi([busy]));
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const picker = await within(sheet).findByRole("group", { name: "Storage" });
    expect(picker).toHaveTextContent("No connection meets it yet");
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    await user.click(within(picker).getByRole("button", { name: "Add a connection" }));
    const adding = await screen.findByRole("dialog", { name: "Add a connection" });
    // It reaches the project this install makes, which the install's plan gives it.
    expect(adding).toHaveTextContent("for WordPress's project");
    // Exactly that project, as a line: more are given on Connections, later (slice 2 review, H2).
    await user.click(within(adding).getByRole("button", { name: /Advanced/ }));
    expect(adding).toHaveTextContent("ReachesWordPress's project, once it's installed");
    expect(within(adding).queryByRole("switch", { name: /Only some projects/ })).not.toBeInTheDocument();
    expect(within(adding).queryByRole("checkbox")).not.toBeInTheDocument();
  });
});

describe("The way the sheet shows is the way it sends (slice 2 review, H1)", () => {
  it("sends the connection the sheet shows, never a busy one listed first", async () => {
    const user = userEvent.setup();
    let asked: InstallCatalogEntryRequest | undefined;
    const change = create(ChangeSchema, { id: "chg_wp", summary: "Install WordPress", state: ChangeState.PLANNED });
    // Required storage: the server lists connections by name, so a busy media comes first.
    const required = create(EntrySchema, {
      ...withStorage,
      needs: withStorage.needs.map((n) => (n.name === "storage" ? create(NeedSchema, { ...n, optional: false }) : n)),
    });
    const base = storageApi([busy, uploads], {
      getCatalogEntry: () => ({
        entry: required,
        project: "wordpress",
        values: { PORT: "8085" },
        parts: [{ service: "wordpress", image: "wordpress:7.1.3-apache", opens: true }],
        needs: [ownMariaDB, busy, uploads],
      }),
      installCatalogEntry: (req) => {
        asked = req;
        return { change };
      },
    });
    renderAt("/catalog/wordpress/install", base);
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Storage" });
    expect(row).toHaveTextContent("Uploads");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.needs["storage"]).toBe("connection:uploads");
  });
});

// WordPress given an optional mail server, as ADR 0099 writes one.
const withMail = create(EntrySchema, {
  ...wordpress,
  needs: [
    ...wordpress.needs,
    create(NeedSchema, {
      name: "mail",
      label: "Mail",
      kind: Kind.SMTP,
      optional: true,
      sets: [
        { name: "SMTP_HOST", part: Part.HOST },
        { name: "SMTP_PASSWORD", part: Part.PASSWORD },
      ],
    }),
  ],
});
const noMail = create(CatalogNeedOptionSchema, { need: "mail", choice: "unset" });
const relay = create(CatalogNeedOptionSchema, {
  need: "mail",
  choice: "connection:relay",
  connection: "relay",
  connectionLabel: "Resend · notes@example.com",
  connectionSender: "notes@example.com",
});
const plain = create(CatalogNeedOptionSchema, {
  need: "mail",
  choice: "connection:plain",
  connection: "plain",
  connectionLabel: "Office relay",
  why: "the app can't be told to require STARTTLS",
});

function mailApi(more: Api["catalog"] = {}): Api {
  const options = [ownMariaDB, noMail, relay, plain];
  const base = api(options, more);
  return {
    ...base,
    catalog: {
      ...base.catalog,
      getCatalogEntry: () => ({
        entry: withMail,
        project: "wordpress",
        values: { PORT: "8085" },
        parts: [{ service: "wordpress", image: "wordpress:7.1.3-apache", opens: true }],
        needs: options,
      }),
      ...more,
    },
    connections: { listConnections: () => ({ connections: [] }) },
  };
}

describe("An app's mail as it's installed (ADR 0099)", () => {
  it("says on its page that it sends mail through a server you pick, with the way to pick one (#792)", async () => {
    renderAt("/catalog/wordpress", mailApi());
    const fact = await screen.findByText("Sends mail through a mail server, if you pick one");
    const row = fact.closest("li");
    if (!row) throw new Error("the fact is no list item");
    expect(within(row).getByRole("link", { name: "Pick one" })).toHaveAttribute("href", "/catalog/wordpress/install");
  });

  it("says in a line that an app that can't go without mail has no mail server yet, with Add a mail server beside it", async () => {
    const needsMail = create(EntrySchema, {
      ...withMail,
      needs: withMail.needs.map((n) => (n.name === "mail" ? create(NeedSchema, { ...n, optional: false }) : n)),
    });
    const options = [ownMariaDB, plain];
    const base = api(options);
    renderAt("/catalog/wordpress/install", {
      ...base,
      catalog: {
        ...base.catalog,
        getCatalogEntry: () => ({
          entry: needsMail,
          project: "wordpress",
          values: { PORT: "8085" },
          parts: [{ service: "wordpress", image: "wordpress:7.1.3-apache", opens: true }],
          needs: options,
        }),
      },
      connections: { listConnections: () => ({ connections: [] }) },
    });
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Mail" });
    expect(row).toHaveTextContent("No mail server yet, and WordPress needs one");
    expect(within(row).getByRole("button", { name: "Add a mail server" })).toBeVisible();
  });

  it("installs with the only mail server that can meet it by default, with no mail a step away, and one that can't off with why", async () => {
    const user = userEvent.setup();
    let asked: InstallCatalogEntryRequest | undefined;
    const change = create(ChangeSchema, { id: "chg_wp", summary: "Install WordPress", state: ChangeState.PLANNED });
    const { container } = renderAt(
      "/catalog/wordpress/install",
      mailApi({
        installCatalogEntry: (req) => {
          asked = req;
          return { change };
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    // The only one that can meet it is chosen at once (#792).
    const row = await within(sheet).findByRole("group", { name: "Mail" });
    expect(row).toHaveTextContent("Resend");
    await user.click(within(row).getByRole("button", { name: "Change mail" }));
    const picker = within(sheet).getByRole("group", { name: "Mail" });
    expect(picker).toHaveTextContent("Any mail server");
    expect(within(picker).getByRole("radio", { name: /Resend/ })).toBeChecked();
    expect(picker).toHaveTextContent("WordPress sends no mail");
    expect(
      within(picker)
        .getByRole("radio", { name: /Resend/ })
        .closest("label"),
    ).toHaveTextContent("Sends as notes@example.com");
    expect(within(picker).getByRole("radio", { name: /Office relay/ })).toBeDisabled();
    expect(picker).toHaveTextContent("The app can't be told to require STARTTLS");
    await expectAccessible(container);
    await user.click(within(picker).getByRole("radio", { name: /Resend/ }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.needs).toEqual({ db: "new:mariadb", mail: "connection:relay" });
  });
});

describe("An app's domain as it's installed (#792)", () => {
  const TUNNEL = "3f2d1c0b-4a5b-4c6d-8e7f-9a0b1c2d3e4f";
  const zoned = (more: Api["catalog"] = {}): Api => ({
    ...api(undefined, more),
    domains: {
      listDomains: () => ({ domains: [] }),
      listDnsProviders: () => ({
        providers: [
          {
            id: "dnp_1",
            name: "cloudflare",
            provider: "cloudflare",
            zones: ["example.com"],
            projects: [],
            strictZones: [],
          },
        ],
      }),
      checkDomain: () => ({
        cloudflare: {
          zone: "example.com",
          provider: "cloudflare",
          state: CloudflareName.FREE,
          way: RouteWay.SERVER_TUNNEL,
          ways: [{ way: RouteWay.SERVER_TUNNEL, tunnel: TUNNEL, tunnelName: "web-1" }, { way: RouteWay.PROXY }],
          sslMode: "full",
        },
      }),
    },
  });

  it("leads with a name under a connected zone, checked live, and plans its route with the install", async () => {
    const user = userEvent.setup();
    let asked: InstallCatalogEntryRequest | undefined;
    const change = create(ChangeSchema, { id: "chg_wp", summary: "Install WordPress", state: ChangeState.PLANNED });
    renderAt(
      "/catalog/wordpress/install",
      zoned({
        installCatalogEntry: (req) => {
          asked = req;
          return { change };
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    expect(await within(sheet).findByRole("combobox", { name: "Zone" })).toHaveValue("example.com");
    expect(within(sheet).getByRole("textbox", { name: "Subdomain" })).toHaveValue("wordpress");
    expect(await within(sheet).findByText("Free in Dockwell and at Cloudflare", {}, { timeout: 3000 })).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.route).toMatchObject({
      domain: "wordpress.example.com",
      tls: "off",
      publish: { way: RouteWay.SERVER_TUNNEL, tunnel: TUNNEL, provider: "cloudflare" },
    });
  });

  it("stays on the server alone, saying why, where the server has no way in, and says why when asked for a domain anyway", async () => {
    const user = userEvent.setup();
    let asked: InstallCatalogEntryRequest | undefined;
    const change = create(ChangeSchema, { id: "chg_wp", summary: "Install WordPress", state: ChangeState.PLANNED });
    const base = zoned({
      installCatalogEntry: (req) => {
        asked = req;
        return { change };
      },
    });
    const off = "web-1 has no public address and no tunnel";
    renderAt("/catalog/wordpress/install", {
      ...base,
      domains: {
        ...base.domains,
        checkDomain: () => ({
          cloudflare: {
            zone: "example.com",
            provider: "cloudflare",
            state: CloudflareName.FREE,
            way: RouteWay.SERVER_TUNNEL,
            ways: [{ way: RouteWay.SERVER_TUNNEL, off }],
            sslMode: "full",
          },
        }),
      },
    });
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Domain" }, { timeout: 3000 });
    expect(row).toHaveTextContent("Only on the server, at its port");
    expect(row).toHaveTextContent(off);
    await user.click(within(row).getByRole("button", { name: "Add a domain" }));
    expect(await within(sheet).findByRole("textbox", { name: "Subdomain" })).toHaveValue("wordpress");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    // Review plan never stops without a word: the reason is under the field.
    await waitFor(() => expect(within(sheet).getAllByText(off).length).toBeGreaterThan(0));
    expect(asked).toBeUndefined();
    await user.click(within(sheet).getByRole("button", { name: "Only on the server instead" }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.route).toBeUndefined();
  });

  it("stays on the server alone where no zone is connected, with a domain a step away", async () => {
    const user = userEvent.setup();
    let asked: InstallCatalogEntryRequest | undefined;
    const change = create(ChangeSchema, { id: "chg_wp", summary: "Install WordPress", state: ChangeState.PLANNED });
    renderAt(
      "/catalog/wordpress/install",
      api(undefined, {
        installCatalogEntry: (req) => {
          asked = req;
          return { change };
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install WordPress" });
    const row = await within(sheet).findByRole("group", { name: "Domain" });
    expect(row).toHaveTextContent("Only on the server, at its port");
    await user.click(within(row).getByRole("button", { name: "Add a domain" }));
    await user.type(within(sheet).getByRole("textbox", { name: /Domain/ }), "blog.example.org");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.route).toMatchObject({ domain: "blog.example.org" });
  });
});
