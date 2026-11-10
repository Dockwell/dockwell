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
  type ServerContainer,
} from "@dockwell/api";
import { useQueries } from "@tanstack/react-query";

import { READ_EVERY_MS } from "../../lib/standing";
import { useMay } from "../auth/may";
import { installIn, useCatalog, useInstalls } from "../catalog/useCatalog";
import { useFleetView } from "../servers/useFleet";

/** Every database, with what each says of itself, read again while a screen shows them. */
export function useDatabases() {
  return useQuery(DatabaseService.method.listDatabases, {}, { refetchInterval: READ_EVERY_MS });
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
};

export function engineName(engine: DatabaseEngine): string {
  return ENGINES[engine];
}

/** Whether an engine has databases and users of its own, as SQL ones do. */
export function speaksSQL(engine: DatabaseEngine): boolean {
  return engine !== DatabaseEngine.REDIS && engine !== DatabaseEngine.VALKEY;
}

/** What the URL of each engine starts with. */
const SCHEMES: Readonly<Record<DatabaseEngine, string>> = {
  [DatabaseEngine.UNSPECIFIED]: "",
  [DatabaseEngine.POSTGRESQL]: "postgres",
  [DatabaseEngine.MYSQL]: "mysql",
  [DatabaseEngine.MARIADB]: "mysql",
  [DatabaseEngine.REDIS]: "redis",
  [DatabaseEngine.VALKEY]: "redis",
};

/** The first user each engine's image makes, where a reading hasn't said. */
const FIRST_USER: Readonly<Record<DatabaseEngine, string>> = {
  [DatabaseEngine.UNSPECIFIED]: "",
  [DatabaseEngine.POSTGRESQL]: "postgres",
  [DatabaseEngine.MYSQL]: "root",
  [DatabaseEngine.MARIADB]: "root",
  [DatabaseEngine.REDIS]: "",
  [DatabaseEngine.VALKEY]: "",
};

/** What a database's first server said, which is the one there is for nearly all. */
export function firstReading(db: Database): DatabaseReading | undefined {
  return db.readings[0];
}

/** What a reading says, where it could say anything. */
export function readOf(reading: DatabaseReading | undefined): DatabaseReading | undefined {
  return reading && reading.unread === "" ? reading : undefined;
}

/** The version a database runs: what it says, or what its image's tag names. */
export function versionOf(db: Database, reading = readOf(firstReading(db))): string {
  return reading?.version || db.version;
}

/** "PostgreSQL 17.2". */
export function engineAndVersion(db: Database, reading = readOf(firstReading(db))): string {
  return `${engineName(db.engine)} ${versionOf(db, reading)}`;
}

/**
 * The URL other services in its environment reach it by, with the password masked:
 * "postgres://postgres:••••@db:5432/postgres". It holds no secret. Unmasked, it has no
 * password at all, for a person to copy and add theirs to.
 */
export function internalURL(db: Database, reading = readOf(firstReading(db)), masked = true): string {
  const scheme = SCHEMES[db.engine];
  const user = reading?.user || FIRST_USER[db.engine];
  const where = `${db.host}:${db.port}`;
  const password = masked ? ":••••" : "";
  if (!speaksSQL(db.engine)) return `${scheme}://${db.passwordSecret && masked ? ":••••@" : ""}${where}`;
  const name = db.engine === DatabaseEngine.POSTGRESQL ? `/${firstDatabase(reading) || "postgres"}` : "";
  return `${scheme}://${user}${password}@${where}${name}`;
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
  const entries = useCatalog().data?.entries ?? [];
  const entry = install?.pin?.entry ?? "";
  const own =
    entry !== "" && entry === db.catalogEntry && entries.some((e) => e.id === entry && e.category === "databases");
  return own ? install : undefined;
}
