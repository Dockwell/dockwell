// SPDX-License-Identifier: AGPL-3.0-only

import { createQueryOptions, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  BackupMethod,
  type BackupPolicyInfo,
  BackupService,
  Connection,
  ContainerService,
  type Database,
  DatabaseEngine,
  type DatabaseReading,
  DatabaseService,
  ResourceService,
  type ServerContainer,
} from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useQueries } from "@tanstack/react-query";

import { READ_EVERY_MS } from "../../lib/standing";
import type { AppHealth } from "../apps/health";
import { useMay } from "../auth/may";
import { installIn, useCatalog, useInstalls } from "../catalog/useCatalog";
import { useFleetView } from "../servers/useFleet";

/** Every database, with what each says of itself, read again while a screen shows them. */
export function useDatabases() {
  return useQuery(DatabaseService.method.listDatabases, {}, { refetchInterval: READ_EVERY_MS });
}

/** Users and logical databases that go with a deleted database, including when its server is silent. */
export function useDatabaseDeleteUsers(db: Database) {
  const scope = db.resource?.scope ?? "";
  const users = useQuery(
    ResourceService.method.listResources,
    { kind: "database.user", scope },
    { enabled: scope !== "" },
  );
  const logicals = useQuery(
    ResourceService.method.listResources,
    { kind: "database.logical", scope },
    { enabled: scope !== "" },
  );
  const databases = useDatabases();
  const only = databases.data?.databases.filter((d) => d.resource?.scope === scope).length === 1;
  const madeUsers = new Set(db.readings.flatMap((r) => r.users.map((u) => u.resourceId)));
  const madeLogicals = new Set(db.readings.flatMap((r) => r.databases.map((d) => d.resourceId)));
  return {
    alongside: [
      ...(users.data?.resources ?? []).filter(
        (u) => u.resource?.kind === "database.user" && (only || madeUsers.has(u.id)),
      ),
      ...(logicals.data?.resources ?? []).filter(
        (l) => l.resource?.kind === "database.logical" && (only || madeLogicals.has(l.id)),
      ),
    ],
    pending: (scope !== "" && (users.isPending || logicals.isPending)) || databases.isPending,
    error: users.error ?? logicals.error ?? databases.error,
    retry: () => {
      if (users.error) void users.refetch();
      if (logicals.error) void logicals.refetch();
      if (databases.error) void databases.refetch();
    },
  };
}

/** One database, by its service's ID, with what it holds. */
export function useDatabase(id: string) {
  return useQuery(DatabaseService.method.getDatabase, { id }, { refetchInterval: READ_EVERY_MS, retry: false });
}

/** What each engine is called. These are names, the same in every language. */
const ENGINES: Readonly<Record<DatabaseEngine, string>> = {
  [DatabaseEngine.UNSPECIFIED]: "",
  [DatabaseEngine.POSTGRESQL]: "PostgreSQL",
  [DatabaseEngine.MYSQL]: "MySQL",
  [DatabaseEngine.MARIADB]: "MariaDB",
  [DatabaseEngine.REDIS]: "Redis",
  [DatabaseEngine.VALKEY]: "Valkey",
  [DatabaseEngine.MONGODB]: "MongoDB",
  [DatabaseEngine.CLICKHOUSE]: "ClickHouse",
};

export function engineName(engine: DatabaseEngine): string {
  return ENGINES[engine];
}

/** Whether an engine has databases and users of its own, as SQL ones and MongoDB do. */
export function speaksSQL(engine: DatabaseEngine): boolean {
  return engine !== DatabaseEngine.REDIS && engine !== DatabaseEngine.VALKEY;
}

/**
 * Whether Dockwell makes users in an engine, each with a database of their own and
 * a password kept as a secret. MongoDB's are listed, and making them comes later
 * (ADR 0038).
 */
export function makesUsers(engine: DatabaseEngine): boolean {
  return engine === DatabaseEngine.POSTGRESQL || engine === DatabaseEngine.MYSQL || engine === DatabaseEngine.MARIADB;
}

/** What the URL of each engine starts with. */
const SCHEMES: Readonly<Record<DatabaseEngine, string>> = {
  [DatabaseEngine.UNSPECIFIED]: "",
  [DatabaseEngine.POSTGRESQL]: "postgres",
  [DatabaseEngine.MYSQL]: "mysql",
  [DatabaseEngine.MARIADB]: "mysql",
  [DatabaseEngine.REDIS]: "redis",
  [DatabaseEngine.VALKEY]: "redis",
  [DatabaseEngine.MONGODB]: "mongodb",
  [DatabaseEngine.CLICKHOUSE]: "clickhouse",
};

/** The first user each engine's image makes, where a reading hasn't said. */
const FIRST_USER: Readonly<Record<DatabaseEngine, string>> = {
  [DatabaseEngine.UNSPECIFIED]: "",
  [DatabaseEngine.POSTGRESQL]: "postgres",
  [DatabaseEngine.MYSQL]: "root",
  [DatabaseEngine.MARIADB]: "root",
  [DatabaseEngine.REDIS]: "",
  [DatabaseEngine.VALKEY]: "",
  [DatabaseEngine.MONGODB]: "",
  [DatabaseEngine.CLICKHOUSE]: "default",
};

/** What a database's first server said, which is the one there is for nearly all. */
export function firstReading(db: Database): DatabaseReading | undefined {
  return db.readings[0];
}

/** What a reading says, where it could say anything. */
export function readOf(reading: DatabaseReading | undefined): DatabaseReading | undefined {
  return reading && reading.unread === "" ? reading : undefined;
}

/** Where a database's interactive console opens: its server and container name, when known. */
export function consoleTarget(db: Database, health?: AppHealth): { serverId: string; container: string } | undefined {
  const runningReading = db.readings.find((r) => r.serverId !== "" && r.container !== "" && r.unread === "");
  if (runningReading) return { serverId: runningReading.serverId, container: runningReading.container };
  const anyReading = db.readings.find((r) => r.serverId !== "" && r.container !== "");
  if (anyReading) return { serverId: anyReading.serverId, container: anyReading.container };
  const server = health?.servers.find((s) => s.serverId !== "" && s.container !== "");
  if (server) return { serverId: server.serverId, container: server.container };
  return undefined;
}

/** A short muted state for a database the agent couldn't read, for a table cell or metric caption. */
export function useUnreadWord() {
  const { t } = useLingui();
  return (why: string | undefined): string => {
    if (!why) return t`Not read yet`;
    if (/refused .*sign-in/i.test(why)) return t`Sign-in refused`;
    if (/isn't running/i.test(why)) return t`Not running`;
    if (/isn't connected/i.test(why)) return t`Server offline`;
    if (/isn't taking connections/i.test(why)) return t`Starting up`;
    if (/didn't answer/i.test(why)) return t`Timed out`;
    if (/has no /i.test(why)) return t`No client tool`;
    if (/older than Dockwell reads/i.test(why)) return t`Too old to read`;
    return t`Not read`;
  };
}

/** The version a database runs: what it says, or what its image's tag names. */
export function versionOf(db: Database, reading = readOf(firstReading(db))): string {
  return reading?.version || db.version;
}

/** "PostgreSQL 17.2". */
export function engineAndVersion(db: Database, reading = readOf(firstReading(db))): string {
  const engine = engineName(db.engine);
  const version = versionOf(db, reading);
  if (!version) return engine;
  return version.toLowerCase().startsWith(engine.toLowerCase()) ? version : `${engine} ${version}`;
}

function buildConnectionURL(
  db: Database,
  reading: DatabaseReading | undefined,
  host: string,
  port: number,
  password: string | undefined,
  http = false,
): string {
  const scheme = http ? "http" : SCHEMES[db.engine];
  const address = host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
  const where = `${address}:${port}`;
  const secret = password === undefined ? "" : `:${encodeURIComponent(password)}`;
  if (db.engine === DatabaseEngine.CLICKHOUSE) {
    const user = encodeURIComponent(firstUser(db, reading));
    const credentials = db.passwordSecret || password !== undefined ? `${user}${secret}@` : "";
    return `${scheme}://${credentials}${where}/default`;
  }
  if (db.engine === DatabaseEngine.MONGODB) {
    // Its root user is in admin, which an app names as where it signs in; one that checks no one takes none.
    const root = firstUser(db, reading);
    if (!root || reading?.checksNoOne) return `${scheme}://${where}`;
    return `${scheme}://${encodeURIComponent(root)}${secret}@${where}/?authSource=admin`;
  }
  if (!speaksSQL(db.engine)) return `${scheme}://${secret ? `${secret}@` : ""}${where}`;
  const user = encodeURIComponent(firstUser(db, reading));
  const name = db.engine === DatabaseEngine.POSTGRESQL ? `/${firstDatabase(reading) || "postgres"}` : "";
  return `${scheme}://${user}${secret}@${where}${name}`;
}

/**
 * The URL a service beside it connects with: with the password where it's given, and
 * with none where it isn't, as a person copies it before revealing the secret.
 */
export function connectionURL(
  db: Database,
  reading: DatabaseReading | undefined,
  password: string | undefined,
): string {
  if (db.engine === DatabaseEngine.CLICKHOUSE) {
    return buildConnectionURL(db, reading, db.host, db.httpPort || 8123, password, true);
  }
  return buildConnectionURL(db, reading, db.host, db.port, password);
}

/**
 * The URL from outside the server, only when external access is on and the server's
 * address is known. Empty otherwise.
 */
export function externalConnectionURL(
  db: Database,
  reading: DatabaseReading | undefined,
  serverAddress: string,
  password: string | undefined,
): string {
  if (!db.externalAccess || !serverAddress) return "";
  return buildConnectionURL(db, reading, serverAddress, db.externalAccess.port || db.port, password);
}

/** The user a database's password is for: the one Dockwell signs in as, or the image's first. */
export function firstUser(db: Database, reading: DatabaseReading | undefined): string {
  return reading?.user || FIRST_USER[db.engine];
}

/** The first database that isn't the server's own, which is the one an app is made to use. */
function firstDatabase(reading: DatabaseReading | undefined): string {
  return reading?.databases.find((d) => !d.system)?.name ?? "";
}

/** A volume's policies that back it up on a schedule, by its environment and name. */
export function policiesOf(db: Database, policies: readonly BackupPolicyInfo[]): BackupPolicyInfo[] {
  return policies.filter((p) => p.environmentId === db.resource?.scope && p.spec?.volume === db.volume);
}

/** The official images Dockwell reads as databases, as Docker Hub names them. */
const IMAGES: Readonly<Record<string, DatabaseEngine>> = {
  postgres: DatabaseEngine.POSTGRESQL,
  mysql: DatabaseEngine.MYSQL,
  mariadb: DatabaseEngine.MARIADB,
  redis: DatabaseEngine.REDIS,
  "valkey/valkey": DatabaseEngine.VALKEY,
  mongo: DatabaseEngine.MONGODB,
  clickhouse: DatabaseEngine.CLICKHOUSE,
  "clickhouse/clickhouse-server": DatabaseEngine.CLICKHOUSE,
};

/** The engine an image runs, as the control plane tells (internal/database.Recognise), or undefined. */
export function engineOfImage(image: string): DatabaseEngine | undefined {
  let repo = image.split("@")[0] ?? "";
  const colon = repo.lastIndexOf(":");
  if (colon > repo.lastIndexOf("/")) repo = repo.slice(0, colon);
  for (const host of ["docker.io/", "index.docker.io/", "registry-1.docker.io/"]) {
    if (repo.startsWith(host)) repo = repo.slice(host.length);
  }
  if (repo.startsWith("library/")) repo = repo.slice("library/".length);
  return IMAGES[repo];
}

/** A container Dockwell didn't make that runs a database it knows, on a server. */
export interface Stranger {
  serverId: string;
  serverName: string;
  container: ServerContainer;
  engine: DatabaseEngine;
}

/** How often the containers Dockwell didn't make are read again: they come and go by hand. */
const STRANGERS_EVERY_MS = 30_000;

/**
 * The containers on connected servers that run a database Dockwell knows, and that it
 * didn't make: each one is a database to take on, which is where a server brought from
 * another panel starts. A server that can't list them says nothing here, as its own page
 * says why.
 */
export function useStrangers(): Stranger[] {
  const transport = useTransport();
  const { servers } = useFleetView();
  // A server's containers are an admin's to read: nobody else is shown one to take on (ADR 0059).
  const allowed = useMay()(ContainerService.method.listContainers).allowed;
  const connected = allowed ? (servers ?? []).filter((s) => s.connection === Connection.CONNECTED) : [];
  const reads = useQueries({
    queries: connected.map((server) => ({
      ...createQueryOptions(ContainerService.method.listContainers, { server: server.id }, { transport }),
      refetchInterval: STRANGERS_EVERY_MS,
      retry: false,
    })),
  });
  return connected.flatMap((server, i) =>
    (reads[i]?.data?.containers ?? []).flatMap((container) => {
      const engine = engineOfImage(container.image);
      return engine === undefined ? [] : [{ serverId: server.id, serverName: server.name, container, engine }];
    }),
  );
}

/** What backing up a database's volume does, as its server says. */
export interface BackupHow {
  /** The server has said. */
  known: boolean;
  /** The server couldn't say. */
  failed: boolean;
  /** It takes the database's own dump, with nothing stopped. */
  dump: boolean;
  /** The database whose dump it takes, such as PostgreSQL. */
  database: string;
}

/**
 * What a backup of a database's volume does, asked of its server as the backup sheet
 * asks: only a known database whose image can sign in to dump takes its own dump, and
 * anything else is a copy of its files, which stops it for an exact one.
 */
export function useBackupHow(db: Database): BackupHow {
  // A volume on several servers is backed up on one, so the one its numbers are from.
  const server = db.readings.length > 1 ? (firstReading(db)?.serverId ?? "") : "";
  const preview = useQuery(
    BackupService.method.previewBackup,
    { volumeId: db.volumeId, server },
    { enabled: db.volumeId !== "", retry: false, staleTime: 60_000 },
  );
  const method = preview.data?.method ?? BackupMethod.UNSPECIFIED;
  return {
    known: method !== BackupMethod.UNSPECIFIED,
    failed: preview.isError,
    dump: method === BackupMethod.DUMP,
    database: preview.data?.database ?? "",
  };
}

/**
 * The catalog install a database is the point of, such as the catalog's PostgreSQL:
 * one whose entry is among the catalog's databases. A database an app's install
 * brings along is that app's to upgrade, from its own page, whatever that does first.
 */
export function useOwnInstall(db: Database) {
  const install = installIn(useInstalls().data?.installs ?? [], db.resource?.scope ?? "");
  const entry = install?.pin?.entry ?? "";
  const entries = useCatalog({ ids: [entry] }, { enabled: entry !== "" }).data?.entries ?? [];
  const own =
    entry !== "" && entry === db.catalogEntry && entries.some((e) => e.id === entry && e.category === "databases");
  return own ? install : undefined;
}
