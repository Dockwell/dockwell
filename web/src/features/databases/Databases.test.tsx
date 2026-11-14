// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  BackupMethod,
  BackupSchema,
  BackupState,
  type ChangeDatabasePasswordRequest,
  ChangeSchema,
  ChangeState,
  CompanionSchema,
  ContainerState,
  type CreateDatabaseUserRequest,
  type Database,
  DatabaseAccountSchema,
  DatabaseEngine,
  DatabaseReadingSchema,
  DatabaseSchema,
  Engine,
  EntrySchema,
  EventKind,
  type ListBackupsRequest,
  type ProposeChangeRequest,
  type Resource,
  ResourceSchema,
  type RestoreBackupRequest,
  Role,
  type SetCatalogCompanionRequest,
  type UpgradeCatalogInstallRequest,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { anApp, ENV, places, projectPlaces, running, SERVER } from "../../test/apps";
import { authState, mira } from "../../test/auth";
import { webPlanned } from "../../test/changes";
import { expectNamesLinked } from "../../test/links";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { gist } from "./DatabaseOverview";
import { balancedColumns } from "./DatabasesPage";
import { connectionURL, engineOfImage } from "./useDatabases";

const now = Date.now();

function aDatabase(init: MessageInitShape<typeof DatabaseSchema> = {}): Database {
  const base: MessageInitShape<typeof DatabaseSchema> = {
    id: "svc_db",
    resource: { kind: "service", scope: ENV, name: "db" },
    engine: DatabaseEngine.POSTGRESQL,
    image: "postgres:17",
    version: "17",
    volume: "data",
    volumeId: "vol_data",
    port: 5432,
    host: "db",
    passwordSecret: "POSTGRES_PASSWORD",
    beside: ["adminer"],
    readings: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        readTime: timestampFromDate(new Date(now)),
        version: "17.2",
        user: "postgres",
        sizeBytes: 9_400_000n,
        connections: 24n,
        maxConnections: 100n,
        uptimeSeconds: 7_200n,
        cacheHit: 0.992,
        databases: [
          { name: "postgres", owner: "postgres", sizeBytes: 7_500_000n, system: true },
          { name: "shop", owner: "shop", sizeBytes: 1_900_000n },
        ],
        users: [
          { name: "postgres", superuser: true, canSignIn: true, databases: ["postgres"] },
          { name: "shop", canSignIn: true, databases: ["shop"] },
        ],
      },
    ],
    newestBackup: {
      id: "bkp_1",
      volume: "data",
      volumeId: "vol_data",
      state: BackupState.DONE,
      method: BackupMethod.DUMP,
      database: "PostgreSQL",
      sizeBytes: 2_400_000n,
      startedAt: timestampFromDate(new Date(now - 3 * 3_600_000)),
      finishedAt: timestampFromDate(new Date(now - 3 * 3_600_000 + 4_000)),
    },
  };
  return create(DatabaseSchema, { ...base, ...init } as MessageInitShape<typeof DatabaseSchema>);
}

const cache = (): Database =>
  aDatabase({
    id: "svc_cache",
    resource: { kind: "service", scope: ENV, name: "cache" },
    engine: DatabaseEngine.REDIS,
    image: "redis:7",
    version: "7",
    volume: "",
    volumeId: "",
    port: 6379,
    host: "cache",
    passwordSecret: "",
    beside: [],
    newestBackup: undefined,
    readings: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        version: "7.4.1",
        sizeBytes: 1_048_576n,
        connections: 2n,
        maxConnections: 10_000n,
        keys: 15n,
        uptimeSeconds: 60n,
      },
    ],
  });

const mongo = (reading: MessageInitShape<typeof DatabaseReadingSchema> = {}): Database =>
  aDatabase({
    id: "svc_docs",
    resource: { kind: "service", scope: ENV, name: "docs" },
    engine: DatabaseEngine.MONGODB,
    image: "mongo:8.0",
    version: "8.0",
    port: 27017,
    host: "docs",
    passwordSecret: "MONGO_INITDB_ROOT_PASSWORD",
    beside: ["shop"],
    newestBackup: undefined,
    readings: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        version: "8.0.34",
        user: "root",
        sizeBytes: 356_352n,
        connections: 3n,
        maxConnections: 209_715n,
        uptimeSeconds: 3_538n,
        featureCompatibility: "7.0",
        databases: [
          { name: "admin", sizeBytes: 151_552n, system: true },
          { name: "shop", sizeBytes: 81_920n },
        ],
        users: [
          { name: "root", authDatabase: "admin", superuser: true, canSignIn: true },
          { name: "shopper", authDatabase: "shop", canSignIn: true, databases: ["shop"] },
        ],
        ...reading,
      },
    ],
  });

const postgres = create(EntrySchema, { id: "postgres", name: "PostgreSQL", category: "databases", version: "17.11" });
const mysql = create(EntrySchema, { id: "mysql", name: "MySQL", category: "databases", version: "8.4.11" });
const tunnel = create(EntrySchema, { id: "cloudflared", name: "Cloudflare Tunnel", category: "networking" });
// A tool that manages databases, among the catalog's databases, which makes none.
const adminer = create(EntrySchema, { id: "adminer", name: "Adminer", category: "databases", version: "6.2.0" });
// Another catalog's PostgreSQL, under another ID, and an app among the databases that
// brings one of its own (#599).
const storePostgres = create(EntrySchema, {
  id: "postgresql",
  name: "PostgreSQL",
  category: "databases",
  version: "18.7-alpine",
});
const metabase = create(EntrySchema, { id: "metabase", name: "Metabase", category: "databases", version: "0.56" });
const ENGINES: Record<string, Engine> = {
  postgres: Engine.POSTGRESQL,
  postgresql: Engine.POSTGRESQL,
  mysql: Engine.MYSQL,
};

/** What installing an entry runs: a database for an engine, an app for a tool, and an app with a database beside it. */
const catalogEntry = (req: { id: string }) => {
  const entry = [postgres, mysql, adminer, tunnel, storePostgres, metabase].find((e) => e.id === req.id) ?? postgres;
  const parts =
    req.id === "adminer"
      ? [{ service: "adminer", image: "adminer:6.2.0", opens: true }]
      : req.id === "metabase"
        ? [
            { service: "metabase", image: "metabase/metabase:0.56", opens: true },
            { service: "db", image: "postgres:17", database: true, engine: Engine.POSTGRESQL, volumes: ["data"] },
          ]
        : [
            {
              service: "db",
              image: `${req.id}:${entry.version}`,
              database: true,
              engine: ENGINES[req.id] ?? Engine.UNSPECIFIED,
              volumes: ["data"],
            },
          ];
  return { entry, project: req.id === "postgres" ? "postgres-2" : req.id, values: {}, parts };
};

/** 1Panel's store as a source, with its PostgreSQL and its Metabase. */
const onePanel = {
  name: "onepanel",
  store: "onepanel",
  source: { repository: "https://github.com/1Panel-dev/appstore", commit: "a".repeat(40) },
  entries: ["postgresql", "metabase"],
};

function api(databases: Database[], more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return {
    servers: { watchServers: watch.stream },
    resources: places(),
    health: running(...databases),
    catalog: {
      listCatalogEntries: () => ({ entries: [tunnel, adminer, postgres, mysql] }),
      getCatalogEntry: catalogEntry,
    },
    backups: { previewBackup: () => ({ method: BackupMethod.DUMP, database: "PostgreSQL" }) },
    databases: {
      listDatabases: () => ({ databases }),
      getDatabase: (req) => {
        const found = databases.find((d) => d.id === req.id);
        if (!found) throw new ConnectError("there's no database", Code.NotFound);
        return { database: found };
      },
    },
    ...more,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the databases list", () => {
  it("lists each database with what it says of itself, and how it's backed up", async () => {
    const { container } = renderAt("/databases", api([cache(), aDatabase()]));
    const table = await screen.findByRole("table", { name: "Databases" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);
    expect(rows[1]).toHaveTextContent("db");
    expect(rows[1]).toHaveTextContent("PostgreSQL 17.2");
    expect(rows[1]).toHaveTextContent("9 MB");
    expect(rows[1]).toHaveTextContent("24 of 100");
    expect(rows[1]).toHaveTextContent("By hand · 3 h ago");
    expect(rows[1]).toHaveTextContent("adminer");
    expect(rows[0]).toHaveTextContent("Redis 7.4.1");
    expect(rows[0]).toHaveTextContent("No volume");
    expect(screen.getByText("2 on hz-1")).toBeVisible();
    await expectAccessible(container);
  });

  it("says where each is in the backups too, where several in different projects share a name", async () => {
    renderAt(
      "/databases",
      api(
        [
          aDatabase({ id: "svc_shop", resource: { kind: "service", scope: "env_shop", name: "db" } }),
          aDatabase({ id: "svc_blog", resource: { kind: "service", scope: "env_blog", name: "db" } }),
        ],
        { resources: projectPlaces("shop", "blog") },
      ),
    );
    const backups = await screen.findByRole("region", { name: "Backups" });
    await waitFor(() =>
      expect([...backups.querySelectorAll(".databases-fact-place")].map((p) => p.textContent)).toEqual([
        "shop",
        "blog",
      ]),
    );
  });

  it("names each app beside a database, and each server it's on, as a link to its page", async () => {
    const adminerApp = anApp("adminer", "adminer:5");
    renderAt(
      "/databases",
      api([cache(), aDatabase()], { services: { listServices: () => ({ services: [adminerApp] }) } }),
    );
    const table = await screen.findByRole("table", { name: "Databases" });
    expect(await within(table).findByRole("link", { name: "adminer" })).toHaveAttribute("href", "/apps/svc_adminer");
    expectNamesLinked(table, ["adminer", "hz-1"]);
  });

  it("shows what happened to them lately, from each one's own timeline", async () => {
    const asked: string[] = [];
    renderAt(
      "/databases",
      api([aDatabase()], {
        events: {
          listEvents: (req) => {
            asked.push(req.resourceId);
            return req.resourceId === "vol_data"
              ? {
                  events: [
                    {
                      id: "evt_1",
                      kind: EventKind.BACKUP,
                      action: "backup.taken",
                      time: timestampFromDate(new Date(now - 60_000)),
                      subject: { id: "vol_data", name: "data" },
                      backup: { id: "bkp_1", volume: "data", serverName: "hz-1", sizeBytes: 2_400_000n },
                    },
                  ],
                }
              : {};
          },
        },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Recent activity" });
    expect(await within(panel).findByText("Backed up data")).toBeVisible();
    expect(within(panel).getByText(/MB · hz-1$/)).toBeVisible();
    expect(asked).toEqual(expect.arrayContaining(["svc_db", "vol_data"]));
  });

  it("offers one tile for each engine, however many catalogs have it, and leaves out the tools and the apps that bring one", async () => {
    const { container } = renderAt(
      "/databases",
      api([aDatabase()], {
        catalog: {
          listCatalogEntries: () => ({ entries: [tunnel, adminer, metabase, postgres, storePostgres, mysql] }),
          getCatalogEntry: catalogEntry,
          listCatalogSources: () => ({ sources: [onePanel] }),
        },
      }),
    );
    const engines = await screen.findByRole("list", { name: "Engines" });
    await waitFor(() => expect(engines).not.toHaveAttribute("aria-busy"));
    const tiles = within(engines).getAllByRole("link");
    expect(tiles.map((l) => l.querySelector(".databases-engine-name")?.textContent)).toEqual(["PostgreSQL", "MySQL"]);
    // Dockwell's own PostgreSQL is the one a tile makes, whatever version another catalog has.
    expect(tiles[0]).toHaveAttribute("href", "/databases?new=postgres");
    expect(tiles[0]).toHaveTextContent("17.11");
    expect(tiles[1]).toHaveAttribute("href", "/databases?new=mysql");
    expect(screen.getByRole("link", { name: "More in the catalog" })).toHaveAttribute(
      "href",
      "/catalog?category=databases",
    );
    expect(screen.getByRole("link", { name: "New database" })).toHaveAttribute("href", "/databases?new=postgres");
    await expectAccessible(container);
  });

  it("picks the version, and the catalog it comes from, in the sheet, Dockwell's own first", async () => {
    const user = userEvent.setup();
    renderAt(
      "/databases?new=postgres",
      api([aDatabase()], {
        catalog: {
          listCatalogEntries: () => ({ entries: [adminer, metabase, postgres, storePostgres, mysql] }),
          getCatalogEntry: catalogEntry,
          listCatalogSources: () => ({ sources: [onePanel] }),
          installCatalogEntry: () => {
            throw new ConnectError("stop here", Code.FailedPrecondition);
          },
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New database" });
    const engine = within(sheet).getByRole("group", { name: "Engine" });
    await waitFor(() => expect(within(engine).getAllByRole("radio")).toHaveLength(2));
    expect(within(engine).queryByRole("radio", { name: /Metabase/ })).not.toBeInTheDocument();
    const version = await within(sheet).findByRole("group", { name: "Version" });
    const versions = within(version).getAllByRole("radio");
    expect(versions.map((r) => r.closest("label")?.textContent)).toEqual([
      expect.stringMatching(/^17\.11Dockwell's own$/),
      expect.stringMatching(/^18\.7-alpine1Panel/),
    ]);
    expect(versions[0]).toBeChecked();
    await user.click(versions[1] as HTMLElement);
    await waitFor(() => expect(within(sheet).getByRole("group", { name: "Name" })).toHaveTextContent("postgresql"));
    // Still PostgreSQL, and still the one sheet.
    expect(within(engine).getByRole("radio", { name: /PostgreSQL/ })).toBeChecked();
    expect(screen.getByRole("dialog", { name: "New database" })).toBe(sheet);
  });

  it("lays the tiles out in even rows at any width", () => {
    expect(balancedColumns(6, 1160, 16)).toBe(6);
    // Four fit, but two rows of three are even, where four and two aren't.
    expect(balancedColumns(6, 760, 16)).toBe(3);
    expect(balancedColumns(7, 760, 16)).toBe(4);
    expect(balancedColumns(4, 1160, 16)).toBe(4);
    expect(balancedColumns(5, 358, 8)).toBe(2);
    expect(balancedColumns(1, 1160, 16)).toBe(1);
    expect(balancedColumns(3, 0, 16)).toBe(1);
  });

  it("makes a new database from its defaults, with its engine picked from a list", async () => {
    const user = userEvent.setup();
    const asked: { id: string; project: string }[] = [];
    const { container } = renderAt(
      "/databases",
      api([aDatabase()], {
        catalog: {
          listCatalogEntries: () => ({ entries: [adminer, postgres, mysql] }),
          getCatalogEntry: catalogEntry,
          installCatalogEntry: (req) => {
            asked.push({ id: req.id, project: req.project });
            throw new ConnectError("stop here", Code.FailedPrecondition);
          },
        },
      }),
    );
    await user.click(await screen.findByRole("link", { name: "New database" }));
    const sheet = await screen.findByRole("dialog", { name: "New database" });
    const engine = within(sheet).getByRole("group", { name: "Engine" });
    await waitFor(() => expect(within(engine).getByRole("radio", { name: /PostgreSQL/ })).toBeChecked());
    expect(within(engine).queryByRole("radio", { name: /Adminer/ })).not.toBeInTheDocument();
    expect(await within(sheet).findByRole("group", { name: "Name" })).toHaveTextContent("postgres-2");
    await expectAccessible(container);

    await user.click(within(engine).getByRole("radio", { name: /MySQL/ }));
    await waitFor(() => expect(within(sheet).getByRole("group", { name: "Name" })).toHaveTextContent("mysql"));
    expect(screen.getByRole("dialog", { name: "New database" })).toBe(sheet);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toEqual([{ id: "mysql", project: "mysql" }]));
  });

  it("keeps the engine as it is while its plan is asked for", async () => {
    const user = userEvent.setup();
    let answer: () => void = () => {};
    renderAt(
      "/databases",
      api([aDatabase()], {
        catalog: {
          listCatalogEntries: () => ({ entries: [adminer, postgres, mysql] }),
          getCatalogEntry: catalogEntry,
          installCatalogEntry: () =>
            new Promise((_, reject) => {
              answer = () => reject(new ConnectError("stop here", Code.FailedPrecondition));
            }),
        },
      }),
    );
    await user.click(await screen.findByRole("link", { name: "New database" }));
    const sheet = await screen.findByRole("dialog", { name: "New database" });
    const engine = within(sheet).getByRole("group", { name: "Engine" });
    await within(sheet).findByRole("group", { name: "Name" });
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(within(engine).getByRole("radio", { name: /MySQL/ })).toBeDisabled());
    answer();
    await waitFor(() => expect(within(engine).getByRole("radio", { name: /MySQL/ })).toBeEnabled());
    expect(within(engine).getByRole("radio", { name: /PostgreSQL/ })).toBeChecked();
  });

  it("offers the engines as the empty state's choices when there's no database yet", async () => {
    const { container } = renderAt("/databases", api([]));
    const empty = (await screen.findByRole("heading", { level: 2, name: "No databases yet" })).closest(
      ".empty-state",
    ) as HTMLElement;
    const engines = await within(empty).findByRole("list", { name: "Engines" });
    await waitFor(() => expect(within(engines).getAllByRole("link")).toHaveLength(2));
    expect(within(empty).getByRole("link", { name: "More in the catalog" })).toBeVisible();
    // The tiles are the way in: no second row of them, and no button beside them.
    expect(screen.queryByRole("heading", { name: "Create a database" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "New database" })).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("names the databases in containers Dockwell didn't make, each a link to where it's taken on", async () => {
    renderAt(
      "/databases",
      api([], {
        containers: {
          listContainers: () => ({
            containers: [
              { id: "c1", name: "pg-legacy", image: "postgres:16-alpine", state: ContainerState.RUNNING },
              { id: "c2", name: "nginx", image: "nginx:1.27", state: ContainerState.RUNNING },
            ],
          }),
        },
      }),
    );
    const note = await screen.findByText("1 database runs in a container Dockwell didn't make");
    const callout = note.closest(".dw-callout") as HTMLElement;
    const link = within(callout).getByRole("link", { name: "pg-legacy" });
    expect(link).toHaveAttribute("href", `/servers/${SERVER}/containers/pg-legacy`);
    expect(within(callout).queryByText("nginx")).toBeNull();
  });

  it("says when a database didn't answer, beside what the others said", async () => {
    const quiet = aDatabase({
      readings: [
        { serverId: SERVER, serverName: "hz-1", unread: "PostgreSQL isn't running, so it can't say how it is" },
      ],
    });
    renderAt("/databases", api([quiet]));
    const table = await screen.findByRole("table", { name: "Databases" });
    const row = within(table).getAllByRole("row")[1] as HTMLElement;
    expect(row).toHaveTextContent("PostgreSQL 17");
    expect(within(row).getAllByText("PostgreSQL isn't running, so it can't say how it is")).not.toHaveLength(0);
  });

  it("opens a database's page from its row", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/databases", api([aDatabase()]));
    const table = await screen.findByRole("table", { name: "Databases" });
    await user.click(within(table).getByText("db"));
    await waitFor(() => expect(router.state.location.pathname).toBe("/databases/svc_db"));
    expect(await screen.findByRole("heading", { level: 1, name: "db" })).toBeVisible();
  });

  it("stacks rows on a phone", async () => {
    onPhone();
    renderAt("/databases", api([aDatabase()]));
    const row = await screen.findByRole("link", { name: /^db/ });
    expect(row).toHaveAttribute("href", "/databases/svc_db");
    expect(row).toHaveTextContent("PostgreSQL 17.2");
    expect(row).toHaveTextContent("9 MB");
  });

  it("reads in Chinese", async () => {
    await activateLocale("zh-CN");
    try {
      renderAt("/databases", api([aDatabase()]));
      expect(await screen.findByRole("heading", { level: 2, name: "创建数据库" })).toBeVisible();
      expect(screen.getByRole("link", { name: "新建数据库" })).toBeVisible();
    } finally {
      await activateLocale("en");
    }
  });
});

describe("a database's page", () => {
  it("shows its numbers, how it's backed up, and what to connect with, its password masked", async () => {
    const { container } = renderAt("/databases/svc_db", api([aDatabase()]));
    expect(await screen.findByRole("heading", { level: 1, name: "db" })).toBeVisible();
    expect(screen.getByText(/PostgreSQL 17\.2 · beside adminer · backed up 3 h ago/)).toBeVisible();
    expect(screen.getByText("24")).toBeVisible();
    expect(screen.getByText("of 100")).toBeVisible();
    expect(screen.getByText("99.2")).toBeVisible();
    const card = screen.getByRole("region", { name: "Connect" });
    expect(within(card).getByText("postgres", { selector: "code" })).toBeVisible();
    expect(within(card).getByText("5432")).toBeVisible();
    expect(within(card).getByText("postgres://postgres:••••••••••••@db:5432/shop")).toBeVisible();
    expect(within(card).getByText("POSTGRES_PASSWORD").closest(".dw-conn-hint")).toHaveTextContent(
      "Kept in the secret POSTGRES_PASSWORD",
    );
    expect(await screen.findByText(/PostgreSQL's own dump, while it runs: nothing stops/)).toBeVisible();
    await expectAccessible(container);
  });

  it("copies its connection string without the password until it's revealed", async () => {
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    renderAt("/databases/svc_db", api([aDatabase()]));
    await user.click(await screen.findByRole("button", { name: "Copy connection string, without the password" }));
    expect(writeText).toHaveBeenCalledWith("postgres://postgres@db:5432/shop");
  });

  it("says where a password kept in its own environment is, with nothing to reveal", async () => {
    renderAt("/databases/svc_db", api([aDatabase({ passwordSecret: "" })]));
    const card = await screen.findByRole("region", { name: "Connect" });
    expect(within(card).getByText(/Its password is in its own environment, sealed/)).toBeVisible();
    expect(within(card).queryByRole("button", { name: "Reveal password" })).toBeNull();
  });

  it("reveals the password from its secret, and copies it in the connection string", async () => {
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    const revealed: string[] = [];
    renderAt(
      "/databases/svc_db",
      api([aDatabase()], {
        variables: {
          listVariables: () => ({
            environmentId: ENV,
            variables: [{ id: "sec_pg", name: "POSTGRES_PASSWORD", secret: true, revision: 1n }],
          }),
          revealSecret: (req: { id: string }) => {
            revealed.push(req.id);
            return { value: "pa ss/word", revision: 1n };
          },
        },
      }),
    );
    const card = await screen.findByRole("region", { name: "Connect" });
    await user.click(await within(card).findByRole("button", { name: "Reveal password" }));
    expect(await within(card).findByText("pa ss/word")).toBeVisible();
    expect(revealed).toEqual(["sec_pg"]);
    expect(within(card).getByText("postgres://postgres:pa%20ss%2Fword@db:5432/shop")).toBeVisible();
    await user.click(within(card).getByRole("button", { name: "Copy connection string" }));
    expect(writeText).toHaveBeenCalledWith("postgres://postgres:pa%20ss%2Fword@db:5432/shop");
    await user.click(within(card).getByRole("button", { name: "Hide" }));
    expect(within(card).queryByText("pa ss/word")).toBeNull();
  });

  it("leaves Reveal in place for a viewer, off, with why", async () => {
    renderAt("/databases/svc_db", {
      ...api([aDatabase()], {
        variables: {
          listVariables: () => ({
            environmentId: ENV,
            variables: [{ id: "sec_pg", name: "POSTGRES_PASSWORD", secret: true, revision: 1n }],
          }),
        },
      }),
      auth: { getAuthState: () => authState({ user: { ...mira, role: Role.VIEWER } }) },
    });
    const card = await screen.findByRole("region", { name: "Connect" });
    const reveal = await within(card).findByRole("button", { name: "Reveal password" });
    await waitFor(() => expect(reveal).toHaveAttribute("aria-disabled", "true"));
    expect(reveal).toHaveAccessibleDescription(/developer or more/);
  });

  it("counts a group that can't sign in as a user, and says so", async () => {
    const db = aDatabase();
    db.readings[0]?.users.push(create(DatabaseAccountSchema, { name: "readers", canSignIn: false }));
    renderAt("/databases/svc_db", api([db]));
    expect(await screen.findByText("3 users of db, 2 of whom can sign in")).toBeVisible();
    const table = screen.getByRole("table", { name: "Users of db" });
    expect(within(table).getByText("readers", { selector: ".databases-name" }).closest("tr")).toHaveTextContent(
      "Can't sign in",
    );
    // None of them is a user Dockwell made, so there's no column of actions to leave empty.
    expect(within(table).queryByRole("columnheader", { name: "Actions" })).toBeNull();
  });

  it("lists who can sign in and the databases in it, leaving out what the server made for itself", async () => {
    renderAt("/databases/svc_db", api([aDatabase()]));
    const users = await screen.findByRole("table", { name: "Users of db" });
    const rows = within(users).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent(/^postgresCan do anythingpostgres/);
    expect(rows[1]).toHaveTextContent(/^shopSigns inshop/);
    const schemas = screen.getByRole("table", { name: "Databases in db" });
    expect(within(schemas).getAllByRole("row")[1]).toHaveTextContent("postgresIts own");
    expect(screen.getByRole("link", { name: /Users/ })).toHaveTextContent("2");
  });

  it("shows a MongoDB with its users' databases, its features and how its backups stand", async () => {
    const { container } = renderAt("/databases/svc_docs", api([mongo()]));
    expect(await screen.findByRole("heading", { level: 1, name: "docs" })).toBeVisible();
    expect(await screen.findByText("mongodb://root:••••••••••••@docs:27017/?authSource=admin")).toBeVisible();
    // What it says of upgrades and backups is facts on a line, not a paragraph.
    expect(
      screen.getByText(
        "Data at 7.0's features, which its next upgrade raises · Runs alone: a backup isn't one moment while something writes",
      ),
    ).toBeVisible();
    const users = screen.getByRole("table", { name: "Users of docs" });
    expect(
      within(users)
        .getAllByRole("row")
        .slice(1)
        .map((r) => r.textContent),
    ).toEqual(["rootadminCan do anythingNone of their own", "shoppershopSigns inshop"]);
    expect(screen.queryByText(/lets anyone in/)).toBeNull();
    // Dockwell makes no MongoDB users yet, so there's nothing to add, and no secret to name.
    expect(screen.queryByRole("button", { name: "Add a user" })).toBeNull();
    expect(within(users).queryByRole("columnheader", { name: "Password in" })).toBeNull();
    // Nor a new password: MongoDB keeps its root user's in its data, which a new secret wouldn't change.
    expect(screen.queryByRole("button", { name: "Change password" })).toBeNull();
    expect(screen.getByText(/Dockwell can't add a MongoDB user yet/)).toHaveTextContent(
      "Dockwell can't add a MongoDB user yet. Add one with db.createUser in mongosh.",
    );
    await expectAccessible(container);
  });

  it("offers no way to add a MongoDB user on its Users tab either", async () => {
    renderAt("/databases/svc_docs/users", api([mongo()]));
    expect(await screen.findByRole("table", { name: "Users of docs" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Add a user" })).toBeNull();
    expect(screen.getByText(/can't add a MongoDB user yet/)).toBeVisible();
  });

  it("warns of a MongoDB that checks no one, and leads to where that's put right", async () => {
    renderAt("/databases/svc_docs", api([mongo({ checksNoOne: true, user: "", users: [], replicaSet: "rs0" })]));
    expect(await screen.findByText("MongoDB lets anyone in")).toBeVisible();
    // A member that checks sign-ins needs a key file, or it won't start.
    expect(screen.getByText(/every member of rs0 shares, make a root user, then set/)).toHaveTextContent(
      "Give it a --keyFile every member of rs0 shares, make a root user, then set MONGO_INITDB_ROOT_USERNAME and MONGO_INITDB_ROOT_PASSWORD.",
    );
    expect(screen.getByRole("link", { name: "Its environment" })).toHaveAttribute("href", "/apps/svc_docs/environment");
    expect(screen.getByText("mongodb://docs:27017")).toBeVisible();
    // No user to sign in as, and no password: the card says so instead of an empty row.
    expect(screen.queryByRole("term", { name: "User" })).toBeNull();
    expect(screen.queryByText("User", { selector: "dt" })).toBeNull();
    expect(
      screen.getByText(/Replica set rs0: each backup is one moment, and can't be restored into a member yet$/),
    ).toBeVisible();
    // It takes no password, so the page doesn't say one is kept sealed.
    expect(screen.getByText("It takes no password, as it checks no one.")).toBeVisible();
    expect(screen.queryByText(/keeps sealed and never shows/)).toBeNull();
  });

  it("says why it can't tell how it is, and keeps the rest", async () => {
    const quiet = aDatabase({
      readings: [
        {
          serverId: SERVER,
          serverName: "hz-1",
          unread: "PostgreSQL refused Dockwell's sign-in with what its environment holds",
        },
      ],
    });
    renderAt("/databases/svc_db", api([quiet]));
    expect(await screen.findByText("PostgreSQL didn't say how it is")).toBeVisible();
    expect(screen.getByText(/refused Dockwell's sign-in/)).toBeVisible();
    expect(screen.getByText("postgres://postgres:••••••••••••@db:5432/postgres")).toBeVisible();
    expect(screen.queryByRole("table", { name: "Users of db" })).toBeNull();
  });

  it("says what a refusal comes to in a line, with the whole of what it said on asking", async () => {
    const user = userEvent.setup();
    const said =
      "can't run PostgreSQL's own tools in its container: OCI runtime exec failed: \"sh -c export PGPASSWORD=...\"";
    renderAt(
      "/databases/svc_db",
      api([aDatabase({ readings: [{ serverId: SERVER, serverName: "hz-1", unread: said }] })]),
    );
    expect(await screen.findByText("Can't run PostgreSQL's own tools in its container")).toBeVisible();
    expect(screen.queryByText(said)).not.toBeVisible();
    await user.click(screen.getByText("What it said"));
    expect(screen.getByText(said)).toBeVisible();
    expect(gist("PostgreSQL isn't running")).toBe("PostgreSQL isn't running");
    expect(gist("it refused. Dockwell signs in as its image does")).toBe("It refused");
  });

  it("says a backup copies its files, when its server says it takes no dump", async () => {
    const db = aDatabase({ engine: DatabaseEngine.MARIADB, image: "mariadb:11", version: "11", port: 3306 });
    renderAt(
      "/databases/svc_db",
      api([db], { backups: { previewBackup: () => ({ method: BackupMethod.COPY, running: ["db"] }) } }),
    );
    expect(await screen.findByText(/A copy of its files, which stops it a moment/)).toBeVisible();
    expect(screen.queryByText(/own dump/)).toBeNull();
  });

  it("says which server its numbers are from, when it runs on several, and asks that one how it backs up", async () => {
    const db = aDatabase();
    const first = db.readings[0];
    if (first) db.readings.push(create(DatabaseReadingSchema, { ...first, serverId: "srv_2", serverName: "hz-2" }));
    renderAt(
      "/databases/svc_db",
      api([db], {
        backups: {
          previewBackup: (req) => {
            if (req.server !== SERVER)
              throw new ConnectError("data is on hz-1 and hz-2. Say which", Code.InvalidArgument);
            return { method: BackupMethod.DUMP, database: "PostgreSQL" };
          },
        },
      }),
    );
    expect(
      await screen.findByText("Its numbers, databases and users are as hz-1 says, one of the 2 servers it runs on."),
    ).toBeVisible();
    expect(await screen.findByText(/PostgreSQL's own dump, while it runs/)).toBeVisible();
  });

  it("says its server didn't say how it backs up, rather than asking for ever", async () => {
    renderAt(
      "/databases/svc_db",
      api([aDatabase()], {
        backups: {
          previewBackup: () => {
            throw new ConnectError("web-1 isn't connected", Code.FailedPrecondition);
          },
        },
      }),
    );
    expect(await screen.findByText("Its server didn't say")).toBeVisible();
    expect(screen.queryByText("Asking its server")).toBeNull();
  });

  it("says when the last backup failed, rather than when the last whole one ran", async () => {
    const db = aDatabase({
      latestBackup: {
        id: "bkp_9",
        state: BackupState.FAILED,
        error: "The disk is full.",
        startedAt: timestampFromDate(new Date(now - 60_000)),
      },
    });
    renderAt("/databases", api([db]));
    const table = await screen.findByRole("table", { name: "Databases" });
    expect(within(table).getAllByRole("row")[1]).toHaveTextContent("By hand · the last failed, 1 min ago");
    expect(screen.getByRole("region", { name: "Backups" })).toHaveTextContent("which failed: The disk is full.");
  });

  it("says there's nothing to back up when its data isn't in a volume", async () => {
    renderAt("/databases/svc_cache", api([cache()]));
    expect(await screen.findByText(/Its data isn't in a volume/)).toBeVisible();
    expect(screen.getByRole("link", { name: "Give it a volume" })).toHaveAttribute("href", "/apps/svc_cache/settings");
    expect(screen.queryByRole("button", { name: "Back up now" })).toBeNull();
    expect(screen.getByText("In memory, with 15 keys")).toBeVisible();
    expect(screen.queryByRole("link", { name: /Users/ })).toBeNull();
  });

  it("says when there's no such database", async () => {
    renderAt("/databases/svc_gone", api([aDatabase()]));
    expect(await screen.findByRole("heading", { level: 2, name: "No database with this ID" })).toBeVisible();
  });
});

describe("what's a database", () => {
  it("reads the official images as the control plane does", () => {
    expect(engineOfImage("postgres:17.2-alpine")).toBe(DatabaseEngine.POSTGRESQL);
    expect(engineOfImage("docker.io/library/mariadb:11@sha256:ab")).toBe(DatabaseEngine.MARIADB);
    expect(engineOfImage("valkey/valkey:8")).toBe(DatabaseEngine.VALKEY);
    expect(engineOfImage("mongo:8.0")).toBe(DatabaseEngine.MONGODB);
    expect(engineOfImage("bitnami/mongodb:8.0")).toBeUndefined();
    expect(engineOfImage("bitnami/postgresql:17")).toBeUndefined();
    expect(engineOfImage("registry.example.com:5000/redis")).toBeUndefined();
  });

  it("writes each connection string with the password given, and without it before", () => {
    expect(connectionURL(aDatabase({ engine: DatabaseEngine.MYSQL, port: 3306, readings: [] }), undefined, "pw")).toBe(
      "mysql://root:pw@db:3306",
    );
    expect(connectionURL(aDatabase({ readings: [] }), undefined, undefined)).toBe(
      "postgres://postgres@db:5432/postgres",
    );
    expect(connectionURL(cache(), undefined, undefined)).toBe("redis://cache:6379");
    expect(connectionURL(aDatabase({ engine: DatabaseEngine.VALKEY, port: 6379 }), undefined, "a:b")).toBe(
      "redis://:a%3Ab@db:6379",
    );
  });
});

describe("a database's backups and upgrades", () => {
  const backup = create(BackupSchema, {
    id: "bkp_7",
    environmentId: ENV,
    volume: "data",
    volumeId: "vol_data",
    serverId: SERVER,
    serverName: "hz-1",
    state: BackupState.DONE,
    onServer: true,
    method: BackupMethod.DUMP,
    database: "PostgreSQL",
    sizeBytes: 2_400_000n,
    startedAt: timestampFromDate(new Date(now - 3_600_000)),
    finishedAt: timestampFromDate(new Date(now - 3_600_000 + 4_000)),
  });
  const planned = create(ChangeSchema, {
    id: "chg_restore",
    summary: "Restore data",
    state: ChangeState.PLANNED,
    plan: { fingerprint: "abc", resources: [] },
  });

  it("lists its volume's backups, each of which restores through a plan", async () => {
    const user = userEvent.setup();
    const asked = vi.fn((req: ListBackupsRequest) => {
      void req;
      return { backups: [backup] };
    });
    const restored = vi.fn((req: RestoreBackupRequest) => {
      void req;
      return { change: planned };
    });
    const { container } = renderAt(
      "/databases/svc_db/backups",
      api([aDatabase()], {
        backups: {
          listBackups: asked,
          restoreBackup: restored,
          previewBackup: () => ({ method: BackupMethod.DUMP, database: "PostgreSQL" }),
        },
        changes: { getChange: () => ({ change: planned }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const table = await screen.findByRole("table", { name: "Backups of db, newest first" });
    expect(within(table).getAllByRole("row")[1]).toHaveTextContent("PostgreSQL dump");
    expect(asked.mock.calls.some(([req]) => req.volumeId === "vol_data")).toBe(true);
    expect(screen.getByText(/PostgreSQL's own dump, while it runs/)).toBeVisible();
    await expectAccessible(container);
    await user.click(within(table).getByRole("button", { name: "Restore" }));
    await waitFor(() =>
      expect(restored).toHaveBeenCalledWith(expect.objectContaining({ id: "bkp_7" }), expect.anything()),
    );
  });

  it("restores its newest backup from the overview", async () => {
    const user = userEvent.setup();
    const restored = vi.fn((req: RestoreBackupRequest) => {
      void req;
      return { change: planned };
    });
    const db = aDatabase();
    if (db.newestBackup) db.newestBackup.onServer = true;
    renderAt(
      "/databases/svc_db",
      api([db], {
        backups: { restoreBackup: restored },
        changes: { getChange: () => ({ change: planned }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Restore the newest" }));
    await waitFor(() =>
      expect(restored).toHaveBeenCalledWith(expect.objectContaining({ id: "bkp_1" }), expect.anything()),
    );
  });

  it("offers the catalog's newer release, as a plan that backs up first", async () => {
    const user = userEvent.setup();
    const upgraded = vi.fn((req: UpgradeCatalogInstallRequest) => {
      void req;
      return { change: planned };
    });
    renderAt(
      "/databases/svc_db",
      api([aDatabase({ catalogEntry: "postgres" })], {
        catalog: {
          listCatalogEntries: () => ({ entries: [postgres] }),
          listCatalogInstalls: () => ({
            installs: [
              {
                projectId: "prj_pg",
                project: "postgres",
                environmentId: ENV,
                name: "PostgreSQL",
                pin: { entry: "postgres", version: "17.11", release: 1 },
                upgrade: { version: "17.12", release: 2 },
              },
            ],
          }),
          upgradeCatalogInstall: upgraded,
        },
        changes: { getChange: () => ({ change: planned }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    expect(await screen.findByText("PostgreSQL 17.12 is out")).toBeVisible();
    expect(screen.getByText(/backs up the data with PostgreSQL's dump first/)).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Upgrade to 17.12" }));
    await waitFor(() =>
      expect(upgraded).toHaveBeenCalledWith(expect.objectContaining({ id: "prj_pg" }), expect.anything()),
    );
  });

  it("adds the admin tool its install left out, from its own page, as a plan", async () => {
    const user = userEvent.setup();
    const set = vi.fn((req: SetCatalogCompanionRequest) => {
      void req;
      return { change: planned };
    });
    const withTools = create(EntrySchema, {
      id: "postgres",
      name: "PostgreSQL",
      category: "databases",
      companions: [
        create(CompanionSchema, {
          service: "adminer",
          label: "Adminer",
          purpose: "Browse and edit tables in your browser",
        }),
      ],
    });
    const { container } = renderAt(
      "/databases/svc_db",
      api([aDatabase({ catalogEntry: "postgres" })], {
        catalog: {
          listCatalogEntries: () => ({ entries: [withTools] }),
          getCatalogEntry: () => ({ entry: withTools }),
          listCatalogInstalls: () => ({
            installs: [
              {
                projectId: "prj_pg",
                project: "postgres",
                environmentId: ENV,
                name: "PostgreSQL",
                pin: { entry: "postgres", version: "17.11", release: 5 },
                companions: { adminer: false },
              },
            ],
          }),
          setCatalogCompanion: set,
        },
        changes: { getChange: () => ({ change: planned }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const tools = await screen.findByRole("region", { name: "Tools" });
    expect(within(tools).getByText("Browse and edit tables in your browser")).toBeVisible();
    // What's beside it goes by the tool's own name, not its service's.
    expect(screen.getByText(/beside Adminer/)).toBeVisible();
    expect(within(tools).getByText("Not installed")).toBeVisible();
    await expectAccessible(container);
    await user.click(within(tools).getByRole("button", { name: "Add Adminer" }));
    await waitFor(() =>
      expect(set).toHaveBeenCalledWith(
        expect.objectContaining({ id: "prj_pg", service: "adminer", on: true }),
        expect.anything(),
      ),
    );
  });

  it("offers no upgrade for a database an app's install brings along, which is that app's to upgrade", async () => {
    renderAt(
      "/databases/svc_db",
      api([aDatabase({ catalogEntry: "nextcloud" })], {
        catalog: {
          listCatalogEntries: () => ({
            entries: [postgres, create(EntrySchema, { id: "nextcloud", name: "Nextcloud", category: "productivity" })],
          }),
          listCatalogInstalls: () => ({
            installs: [
              {
                projectId: "prj_nc",
                project: "nextcloud",
                environmentId: ENV,
                name: "Nextcloud",
                pin: { entry: "nextcloud", version: "31.0", release: 1 },
                upgrade: { version: "32.0", release: 2 },
              },
            ],
          }),
        },
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "db" });
    expect(screen.queryByText(/is out/)).toBeNull();
    expect(screen.queryByRole("button", { name: /Upgrade to/ })).toBeNull();
  });

  it("says an upgrade copies its files first, when its server says it takes no dump", async () => {
    renderAt(
      "/databases/svc_db",
      api([aDatabase({ catalogEntry: "postgres" })], {
        backups: { previewBackup: () => ({ method: BackupMethod.COPY }) },
        catalog: {
          listCatalogEntries: () => ({ entries: [postgres] }),
          listCatalogInstalls: () => ({
            installs: [
              {
                projectId: "prj_pg",
                project: "postgres",
                environmentId: ENV,
                name: "PostgreSQL",
                pin: { entry: "postgres", version: "17.11", release: 1 },
                upgrade: { version: "17.12", release: 2 },
              },
            ],
          }),
        },
      }),
    );
    expect(await screen.findByText(/Its plan backs up its volumes first/)).toBeVisible();
    expect(screen.queryByText(/own dump/)).toBeNull();
  });

  it("says on its Backups tab why there are none with no volume, and lists no other volume's", async () => {
    const listed = vi.fn(() => ({ backups: [] }));
    renderAt(
      "/databases/svc_cache/backups",
      api([cache()], { backups: { listBackups: listed, previewBackup: () => ({}) } }),
    );
    expect(await screen.findByText(/Its data isn't in a volume, so it goes with its container/)).toBeVisible();
    expect(listed).not.toHaveBeenCalled();
  });

  it("says each backup copies its files, on its Backups tab, when its server says it takes no dump", async () => {
    renderAt(
      "/databases/svc_db/backups",
      api([aDatabase()], {
        backups: { listBackups: () => ({ backups: [] }), previewBackup: () => ({ method: BackupMethod.COPY }) },
      }),
    );
    expect(await screen.findByText(/A copy of its files, which stops PostgreSQL a moment/)).toBeVisible();
    expect(screen.queryByText(/own dump/)).toBeNull();
  });

  it("has no backups tab when its data isn't in a volume", async () => {
    renderAt("/databases/svc_cache", api([cache()]));
    await screen.findByRole("heading", { level: 1, name: "cache" });
    const tabs = screen.getByRole("navigation", { name: "Database sections" });
    expect(within(tabs).queryByRole("link", { name: "Backups" })).toBeNull();
  });
});

describe("adding a user", () => {
  const planned = create(ChangeSchema, {
    id: "chg_user",
    summary: "Add the user orders to db, with the database orders",
    state: ChangeState.PLANNED,
    plan: { fingerprint: "abc", resources: [] },
  });

  it("asks only for a name, checked as it's typed, and plans a user with a database and a password of their own", async () => {
    const user = userEvent.setup();
    const asked = vi.fn((req: CreateDatabaseUserRequest) => {
      void req;
      return { change: planned };
    });
    renderAt(
      "/databases/svc_db/users",
      api([aDatabase()], {
        databases: {
          listDatabases: () => ({ databases: [aDatabase()] }),
          getDatabase: () => ({ database: aDatabase() }),
          createDatabaseUser: asked,
        },
        changes: { getChange: () => ({ change: planned }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a user" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a user to db" });
    // Nothing else is asked: its database and secret are defaults, under Advanced.
    expect(within(sheet).queryByLabelText(/Secret for the password/)).not.toBeInTheDocument();
    await user.type(within(sheet).getByLabelText("User"), "Orders");
    expect(await within(sheet).findByText(/Use up to 32 lowercase letters/)).toBeVisible();
    await user.clear(within(sheet).getByLabelText("User"));
    await user.type(within(sheet).getByLabelText("User"), "orders");
    const advanced = within(sheet).getByRole("button", { name: "Advanced" });
    expect(advanced).toHaveAccessibleDescription("Database orders · secret ORDERS_DATABASE_PASSWORD");
    expect(asked).not.toHaveBeenCalled();
    // A user it has already says so, and so does the database of their name, under an Advanced that opens for it.
    await user.clear(within(sheet).getByLabelText("User"));
    await user.type(within(sheet).getByLabelText("User"), "shop");
    expect(await within(sheet).findByText("shop signs in to db already.")).toBeVisible();
    expect(within(sheet).getByText("db has a database called shop already.")).toBeVisible();
    await user.clear(within(sheet).getByLabelText("User"));
    await user.type(within(sheet).getByLabelText("User"), "orders");
    expect(within(sheet).getByLabelText(/Secret for the password/)).toHaveAttribute(
      "placeholder",
      "ORDERS_DATABASE_PASSWORD",
    );
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() =>
      expect(asked).toHaveBeenCalledWith(
        expect.objectContaining({ id: "svc_db", user: "orders", database: "", secret: "" }),
        expect.anything(),
      ),
    );
  });

  it("says that inside its container PostgreSQL lets anything sign in without a password", async () => {
    renderAt("/databases/svc_db/users", api([aDatabase()]));
    expect(
      await screen.findByText(/Inside its own container, PostgreSQL's image lets anything sign in as any user/),
    ).toBeVisible();
  });

  it("says a default that's taken beside it, as it's typed, under an Advanced that opens for it", async () => {
    const user = userEvent.setup();
    renderAt(
      "/databases/svc_db/users",
      api([aDatabase()], {
        databases: { getDatabase: () => ({ database: aDatabase() }) },
        variables: {
          listVariables: () => ({
            environmentId: ENV,
            variables: [{ id: "sec_orders", name: "ORDERS_DATABASE_PASSWORD", secret: true, revision: 1n }],
          }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a user" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a user to db" });
    await user.type(within(sheet).getByLabelText("User"), "orders");
    await waitFor(() =>
      expect(within(sheet).getByLabelText(/Secret for the password/)).toHaveAccessibleDescription(
        /ORDERS_DATABASE_PASSWORD is a secret already/,
      ),
    );
    await user.type(within(sheet).getByLabelText(/Secret for the password/), "ORDERS_PASSWORD");
    expect(within(sheet).getByLabelText(/Secret for the password/)).not.toHaveAttribute("aria-invalid", "true");
  });

  it("copies the same as a CLI command, under Advanced", async () => {
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    renderAt(
      "/databases/svc_db/users",
      api([aDatabase()], { databases: { getDatabase: () => ({ database: aDatabase() }) } }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a user" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a user to db" });
    await user.type(within(sheet).getByLabelText("User"), "orders");
    await user.click(within(sheet).getByRole("button", { name: "Advanced" }));
    await user.click(within(sheet).getByRole("button", { name: "Copy as CLI command" }));
    expect(writeText).toHaveBeenCalledWith("dockwell databases add-user db --user orders");
  });

  it("says a refusal of one field beside it, and anything else above the buttons", async () => {
    const user = userEvent.setup();
    let reason = "invalid_secret";
    const asked = vi.fn(() => {
      const err = new ConnectError(
        reason === "invalid_secret"
          ? "there's a secret ORDERS_DATABASE_PASSWORD in this environment already"
          : "db runs on no server now",
        Code.InvalidArgument,
      );
      err.metadata.set("Dockwell-Error", reason);
      throw err;
    });
    renderAt(
      "/databases/svc_db/users",
      api([aDatabase()], { databases: { getDatabase: () => ({ database: aDatabase() }), createDatabaseUser: asked } }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a user" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a user to db" });
    await user.type(within(sheet).getByLabelText("User"), "orders");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() =>
      expect(within(sheet).getByLabelText(/Secret for the password/)).toHaveAccessibleDescription(
        /There's a secret ORDERS_DATABASE_PASSWORD/,
      ),
    );
    expect(within(sheet).getByLabelText("User")).not.toHaveAttribute("aria-invalid", "true");
    reason = "invalid";
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(await within(sheet).findByText("Db runs on no server now.")).toBeVisible();
    expect(within(sheet).getByLabelText("User")).not.toHaveAttribute("aria-invalid", "true");
  });

  it("says which secret a user Dockwell made signs in with, and which apps use it", async () => {
    const db = aDatabase();
    const reading = db.readings[0];
    if (reading) {
      reading.users.push(
        create(DatabaseAccountSchema, {
          name: "orders",
          canSignIn: true,
          databases: ["orders"],
          secret: "ORDERS_DATABASE_PASSWORD",
          usedBy: ["api"],
        }),
      );
    }
    renderAt("/databases/svc_db/users", api([db]));
    const table = await screen.findByRole("table", { name: "Users of db" });
    const row = within(table).getByText("orders", { selector: ".databases-name" }).closest("tr") as HTMLElement;
    expect(row).toHaveTextContent("ORDERS_DATABASE_PASSWORD");
    expect(row).toHaveTextContent("api");
    expect(within(table).getByText("shop", { selector: ".databases-name" }).closest("tr")).toHaveTextContent(
      "Not one of Dockwell's",
    );
  });
});

describe("changing a password", () => {
  const planned = create(ChangeSchema, {
    id: "chg_password",
    summary: "Change the password of postgres in db",
    state: ChangeState.PLANNED,
    plan: { fingerprint: "abc", resources: [] },
  });

  it("plans a new password for its first user from beside the password, with nothing to type", async () => {
    const user = userEvent.setup();
    const asked = vi.fn((req: ChangeDatabasePasswordRequest) => {
      void req;
      return { change: planned };
    });
    renderAt(
      "/databases/svc_db",
      api([aDatabase()], {
        databases: {
          listDatabases: () => ({ databases: [aDatabase()] }),
          getDatabase: () => ({ database: aDatabase() }),
          changeDatabasePassword: asked,
        },
        changes: { getChange: () => ({ change: planned }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const card = await screen.findByRole("region", { name: "Connect" });
    await user.click(within(card).getByRole("button", { name: "Change password" }));
    await waitFor(() =>
      expect(asked).toHaveBeenCalledWith(expect.objectContaining({ id: "svc_db", user: "" }), expect.anything()),
    );
    expect(await screen.findByText("Change the password of postgres in db")).toBeVisible();
  });

  it("plans one for a user Dockwell made, from their row, and offers none for one it didn't", async () => {
    const user = userEvent.setup();
    const asked = vi.fn((req: ChangeDatabasePasswordRequest) => {
      void req;
      return { change: planned };
    });
    const db = aDatabase();
    db.readings[0]?.users.push(
      create(DatabaseAccountSchema, {
        name: "orders",
        canSignIn: true,
        databases: ["orders"],
        secret: "ORDERS_DATABASE_PASSWORD",
        resourceId: "dbu_orders",
      }),
    );
    renderAt(
      "/databases/svc_db/users",
      api([db], {
        databases: {
          listDatabases: () => ({ databases: [db] }),
          getDatabase: () => ({ database: db }),
          changeDatabasePassword: asked,
        },
        changes: { getChange: () => ({ change: planned }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const table = await screen.findByRole("table", { name: "Users of db" });
    expect(within(table).getByRole("columnheader", { name: "Actions" })).toBeInTheDocument();
    expect(within(table).queryByRole("button", { name: "Change shop's password" })).toBeNull();
    await user.click(within(table).getByRole("button", { name: "Change orders's password" }));
    await waitFor(() =>
      expect(asked).toHaveBeenCalledWith(expect.objectContaining({ id: "svc_db", user: "orders" }), expect.anything()),
    );
  });

  it("leaves Change password and Reveal in place for a viewer, off, with why", async () => {
    renderAt("/databases/svc_db", {
      ...api([aDatabase()]),
      auth: { getAuthState: () => authState({ user: { ...mira, role: Role.VIEWER } }) },
    });
    const card = await screen.findByRole("region", { name: "Connect" });
    const change = await within(card).findByRole("button", { name: "Change password" });
    await waitFor(() => expect(change).toHaveAttribute("aria-disabled", "true"));
    expect(change).toHaveAccessibleDescription(/developer or more/);
    const reveal = within(card).getByRole("button", { name: "Reveal password" });
    expect(reveal).toHaveAttribute("aria-disabled", "true");
    expect(reveal).toHaveAccessibleDescription(/developer or more/);
  });

  it("offers it on a phone too, on the user's row", async () => {
    const user = userEvent.setup();
    onPhone();
    const asked = vi.fn((req: ChangeDatabasePasswordRequest) => {
      void req;
      return { change: planned };
    });
    const db = aDatabase();
    db.readings[0]?.users.push(
      create(DatabaseAccountSchema, {
        name: "orders",
        canSignIn: true,
        secret: "ORDERS_DATABASE_PASSWORD",
        resourceId: "dbu_orders",
      }),
    );
    renderAt(
      "/databases/svc_db/users",
      api([db], {
        databases: {
          listDatabases: () => ({ databases: [db] }),
          getDatabase: () => ({ database: db }),
          changeDatabasePassword: asked,
        },
        changes: { getChange: () => ({ change: planned }), watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Change password" }));
    await waitFor(() =>
      expect(asked).toHaveBeenCalledWith(expect.objectContaining({ user: "orders" }), expect.anything()),
    );
  });
});

describe("deleting a database", () => {
  const withUser = () => {
    const db = aDatabase();
    const users = db.readings[0]?.users ?? [];
    const shop = users.find((u) => u.name === "shop");
    if (shop) shop.resourceId = "dbu_shop";
    return db;
  };
  const shopUser = create(ResourceSchema, {
    id: "dbu_shop",
    resource: { kind: "database.user", scope: ENV, name: "shop" },
  });
  const service = anApp(
    "db",
    "postgres:17",
    {},
    { id: "svc_db", resource: { kind: "service", scope: ENV, name: "db" } },
  );

  function deleting(proposals: ProposeChangeRequest[], resources?: Api["resources"]): Api {
    const base = places();
    return api([withUser()], {
      resources: resources ?? {
        listResources: (req) =>
          req.kind === "database.user" ? { resources: [shopUser] } : base.listResources(req as { kind: string }),
      },
      services: { listServices: () => ({ services: [service] }) },
      changes: {
        proposeChange: (req) => {
          proposals.push(req);
          return { change: webPlanned() };
        },
        watchChange: feed<WatchChangeResponse>().stream,
        discardChange: () => ({}),
      },
    });
  }
  const edits = (req: ProposeChangeRequest | undefined) =>
    req?.edits.map((e) => [e.resource?.kind, e.resource?.name, e.delete]) ?? [];

  it("takes the users Dockwell made in it, and keeps its volume with what's in it", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/databases/svc_db", deleting(proposals));
    const danger = await screen.findByRole("region", { name: "Danger zone" });
    expect(danger).toHaveTextContent("Its volume stays, with every database and user in it.");
    await user.click(within(danger).getByRole("button", { name: "Delete db" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(edits(proposals[0])).toEqual([
      ["database.user", "shop", true],
      ["service", "db", true],
    ]);
  });

  it("takes its project with it when it's the last thing there, behind a switch that's on", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const project = create(ResourceSchema, { id: "prj_pg", resource: { kind: "project", scope: "org_1", name: "pg" } });
    const others = create(ResourceSchema, {
      id: "prj_1",
      resource: { kind: "project", scope: "org_1", name: "default" },
    });
    const env = create(ResourceSchema, {
      id: ENV,
      resource: { kind: "environment", scope: "prj_pg", name: "production" },
    });
    const inside: Resource[] = [
      create(ResourceSchema, { id: "svc_db", resource: { kind: "service", scope: ENV, name: "db" } }),
      shopUser,
      create(ResourceSchema, { id: "vol_data", resource: { kind: "volume", scope: ENV, name: "data" } }),
    ];
    renderAt(
      "/databases/svc_db",
      deleting(proposals, {
        listResources: (req) => ({
          resources:
            req.kind === "project"
              ? [others, project]
              : req.kind === "environment"
                ? [env]
                : req.kind === "database.user"
                  ? [shopUser]
                  : req.scope === ENV
                    ? inside
                    : [],
        }),
      }),
    );
    const danger = await screen.findByRole("region", { name: "Danger zone" });
    // Its users go with it, as the database's own delete takes them, so what else goes is the volume.
    await waitFor(() => expect(danger).toHaveTextContent("Its project pg goes too, with 1 volume."));
    await user.click(within(danger).getByRole("button", { name: "Delete db" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(within(sheet).getByRole("switch", { name: "Also delete the project pg" })).toBeChecked();
    // One edit, which the engine fills with what's in the project as it plans (ADR 0074).
    expect(proposals[0]?.edits.map((e) => [e.resource?.kind, e.resource?.name, e.delete, e.withContents])).toEqual([
      ["project", "pg", true, true],
    ]);
  });
});
