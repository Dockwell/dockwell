// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  BackupMethod,
  BackupSchema,
  BackupState,
  ChangeSchema,
  ChangeState,
  ContainerState,
  type CreateDatabaseUserRequest,
  type Database,
  DatabaseAccountSchema,
  DatabaseEngine,
  DatabaseReadingSchema,
  DatabaseSchema,
  EntrySchema,
  EventKind,
  type ListBackupsRequest,
  type RestoreBackupRequest,
  type UpgradeCatalogInstallRequest,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { ENV, places, running, SERVER } from "../../test/apps";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { engineOfImage, internalURL } from "./useDatabases";

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

function api(databases: Database[], more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return {
    servers: { watchServers: watch.stream },
    resources: places(),
    health: running(...databases),
    catalog: { listCatalogEntries: () => ({ entries: [tunnel, postgres, mysql] }) },
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

  it("offers the catalog's databases, each a way to make one", async () => {
    renderAt("/databases", api([aDatabase()]));
    const engines = await screen.findByRole("list", { name: "Engines" });
    const links = within(engines).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual(["PostgreSQL17.11", "MySQL8.4.11"]);
    expect(links[0]).toHaveAttribute("href", "/catalog/postgres");
    expect(screen.getByRole("link", { name: "New database" })).toHaveAttribute("href", "/catalog?category=databases");
  });

  it("says what a database is when there's none, and where to get one", async () => {
    const { container } = renderAt("/databases", api([]));
    expect(await screen.findByRole("heading", { level: 2, name: "No databases yet" })).toBeVisible();
    expect(screen.getByText(/runs PostgreSQL, MySQL, MariaDB, Redis or Valkey from its official image/)).toBeVisible();
    expect(screen.getByRole("link", { name: "New database" })).toBeVisible();
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
  it("shows its numbers, how it's backed up, and its connection string with the password masked", async () => {
    const { container } = renderAt("/databases/svc_db", api([aDatabase()]));
    expect(await screen.findByRole("heading", { level: 1, name: "db" })).toBeVisible();
    expect(screen.getByText(/PostgreSQL 17\.2 · beside adminer · backed up 3 h ago/)).toBeVisible();
    expect(screen.getByText("24")).toBeVisible();
    expect(screen.getByText("of 100")).toBeVisible();
    expect(screen.getByText("99.2")).toBeVisible();
    const url = screen.getByLabelText("Internal URL");
    expect(url).toHaveValue("postgres://postgres:••••@db:5432/shop");
    expect(screen.getByText(/The password is in the secret/)).toHaveTextContent("POSTGRES_PASSWORD");
    expect(await screen.findByText(/PostgreSQL's own dump, while it runs: nothing stops/)).toBeVisible();
    expect(screen.getByRole("switch", { name: "External access" })).not.toBeChecked();
    await expectAccessible(container);
  });

  it("copies its URL without the password", async () => {
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    renderAt("/databases/svc_db", api([aDatabase()]));
    await user.click(await screen.findByRole("button", { name: "Copy the internal URL, without its password" }));
    expect(writeText).toHaveBeenCalledWith("postgres://postgres@db:5432/shop");
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
    expect(screen.getByLabelText("Internal URL")).toHaveValue("mongodb://root:••••@docs:27017/?authSource=admin");
    expect(screen.getByText(/It runs 8\.0\.34 with 7\.0's features, so 7\.0 can still run its data/)).toBeVisible();
    expect(screen.getByText(/It runs alone, without a replica set, so a backup isn't one moment/)).toBeVisible();
    const users = screen.getByRole("table", { name: "Users of docs" });
    expect(
      within(users)
        .getAllByRole("row")
        .slice(1)
        .map((r) => r.textContent),
    ).toEqual(["rootadminCan do anythingNone of their own", "shoppershopSigns inshop"]);
    expect(screen.queryByText(/checks no one/)).toBeNull();
    await expectAccessible(container);
  });

  it("warns of a MongoDB that checks no one, and says how to close it", async () => {
    renderAt("/databases/svc_docs", api([mongo({ checksNoOne: true, user: "", users: [], replicaSet: "rs0" })]));
    expect(await screen.findByText("MongoDB checks no one")).toBeVisible();
    expect(screen.getByText(/anything that reaches its port signs in as anyone/)).toBeVisible();
    expect(screen.getByLabelText("Internal URL")).toHaveValue("mongodb://docs:27017");
    expect(screen.getByText(/member of the replica set rs0, so each backup is one moment/)).toBeVisible();
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
    expect(screen.getByLabelText("Internal URL")).toHaveValue("postgres://postgres:••••@db:5432/postgres");
    expect(screen.queryByRole("table", { name: "Users of db" })).toBeNull();
  });

  it("says a backup copies its files, when its server says it takes no dump", async () => {
    const db = aDatabase({ engine: DatabaseEngine.MARIADB, image: "mariadb:11", version: "11", port: 3306 });
    renderAt(
      "/databases/svc_db",
      api([db], { backups: { previewBackup: () => ({ method: BackupMethod.COPY, running: ["db"] }) } }),
    );
    expect(await screen.findByText(/A copy of its files, which stops it for an exact copy/)).toBeVisible();
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

  it("masks the password in every URL", () => {
    expect(internalURL(aDatabase({ engine: DatabaseEngine.MYSQL, port: 3306, readings: [] }))).toBe(
      "mysql://root:••••@db:3306",
    );
    expect(internalURL(aDatabase({ readings: [] }), undefined, false)).toBe("postgres://postgres@db:5432/postgres");
    expect(internalURL(cache())).toBe("redis://cache:6379");
    expect(internalURL(aDatabase({ engine: DatabaseEngine.VALKEY, port: 6379 }))).toBe("redis://:••••@db:6379");
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
    expect(screen.getByText(/PostgreSQL's own dump of everything in it/)).toBeVisible();
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
    expect(screen.getByText(/backs up its data with PostgreSQL's own dump before anything else/)).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Upgrade to 17.12" }));
    await waitFor(() =>
      expect(upgraded).toHaveBeenCalledWith(expect.objectContaining({ id: "prj_pg" }), expect.anything()),
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
    expect(await screen.findByText(/with a copy of their files, which stops PostgreSQL for a moment/)).toBeVisible();
    expect(screen.queryByText(/own dump/)).toBeNull();
  });

  it("says on its Backups tab why there are none with no volume, and lists no other volume's", async () => {
    const listed = vi.fn(() => ({ backups: [] }));
    renderAt(
      "/databases/svc_cache/backups",
      api([cache()], { backups: { listBackups: listed, previewBackup: () => ({}) } }),
    );
    expect(
      await screen.findByText(/Its data isn't in a volume, so it goes when its container is replaced/),
    ).toBeVisible();
    expect(listed).not.toHaveBeenCalled();
  });

  it("says each backup copies its files, on its Backups tab, when its server says it takes no dump", async () => {
    renderAt(
      "/databases/svc_db/backups",
      api([aDatabase()], {
        backups: { listBackups: () => ({ backups: [] }), previewBackup: () => ({ method: BackupMethod.COPY }) },
      }),
    );
    expect(await screen.findByText(/Each backup is a copy of its files, which stops PostgreSQL/)).toBeVisible();
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

  it("asks only for a name, and plans a user with a database and a password of their own", async () => {
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
    await user.type(within(sheet).getByLabelText("User"), "Orders");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(await within(sheet).findByText(/Use up to 32 lowercase letters/)).toBeVisible();
    expect(asked).not.toHaveBeenCalled();
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
      await screen.findByText(
        /Inside its own container, PostgreSQL's official image lets anything sign in as any user/,
      ),
    ).toBeVisible();
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
