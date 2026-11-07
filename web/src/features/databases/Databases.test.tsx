// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  BackupMethod,
  BackupState,
  ContainerState,
  type Database,
  DatabaseEngine,
  DatabaseSchema,
  EntrySchema,
  EventKind,
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
    expect(screen.getByText(/PostgreSQL's own dump, while it runs: nothing stops/)).toBeVisible();
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

  it("lists who can sign in and the databases in it, leaving out what the server made for itself", async () => {
    renderAt("/databases/svc_db", api([aDatabase()]));
    const users = await screen.findByRole("table", { name: "Users of db" });
    const rows = within(users).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.textContent)).toEqual(["postgresCan do anythingpostgres", "shopSigns inshop"]);
    const schemas = screen.getByRole("table", { name: "Databases in db" });
    expect(within(schemas).getAllByRole("row")[1]).toHaveTextContent("postgresIts own");
    expect(screen.getByRole("link", { name: /Users/ })).toHaveTextContent("2");
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
