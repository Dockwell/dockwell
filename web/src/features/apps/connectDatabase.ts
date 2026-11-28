// SPDX-License-Identifier: AGPL-3.0-only

import {
  type Database,
  DatabaseEngine,
  DeploymentState,
  type EnvironmentVariable,
  LinkPart,
  type ServiceInfo,
} from "@dockwell/api";

import { firstReading, readOf } from "../databases/useDatabases";
import { environmentOf } from "./app";

/** What an app connects to here: PostgreSQL, MySQL, MariaDB and ClickHouse (ADR 0084, Connect a database). */
export const CONNECTABLE: readonly DatabaseEngine[] = [
  DatabaseEngine.POSTGRESQL,
  DatabaseEngine.MYSQL,
  DatabaseEngine.MARIADB,
  DatabaseEngine.CLICKHOUSE,
];

/** Where a service runs, by server ID. */
export function serversOf(service: ServiceInfo): string[] {
  return service.deployments.filter((d) => d.state !== DeploymentState.SUPERSEDED).map((d) => d.serverId);
}

/**
 * The database servers an app may connect to: one Dockwell knows, of an engine it
 * connects to, that runs on every server the app runs on, as both ends are on one
 * server for now (#573). One in the app's own environment comes first, as an
 * imported app reaches its database there by name.
 */
export function candidates(databases: readonly Database[], service: ServiceInfo): Database[] {
  const on = serversOf(service);
  const fits = databases.filter((db) => {
    if (!CONNECTABLE.includes(db.engine) || on.length === 0) return false;
    if (db.engine === DatabaseEngine.CLICKHOUSE && !db.passwordSecret && db.resource?.scope !== environmentOf(service))
      return false;
    const runs = db.readings.map((r) => r.serverId);
    return on.every((s) => runs.includes(s));
  });
  const here = environmentOf(service);
  return [...fits.filter((db) => db.resource?.scope === here), ...fits.filter((db) => db.resource?.scope !== here)];
}

/** What a server says it holds: the databases an app may use, their owners, and who may sign in but its administrators. */
export interface Holds {
  databases: string[];
  owners: Map<string, string>;
  users: string[];
}

/**
 * What a server's first reading that read says it holds, or nothing where none has,
 * but what's kept for another app already: a user Dockwell keeps there, and the
 * database such a user owns, as a link's dumps and restores go by it (ADR 0084).
 */
export function holds(db: Database | undefined): Holds {
  const reading = db ? readOf(firstReading(db)) : undefined;
  const out: Holds = { databases: [], owners: new Map(), users: [] };
  const kept = new Set((reading?.users ?? []).filter((u) => u.resourceId !== "").map((u) => u.name));
  for (const d of reading?.databases ?? []) {
    if (d.system || kept.has(d.owner)) continue;
    out.databases.push(d.name);
    out.owners.set(d.name, d.owner);
  }
  for (const u of reading?.users ?? []) {
    if (u.canSignIn && !u.superuser && !kept.has(u.name)) out.users.push(u.name);
  }
  return out;
}

/** The names of its environment an app reads, each a variable or a secret. */
export interface AppName {
  name: string;
  secret: boolean;
}

/** The names of its environment an app reads, as the environment says who uses each. */
export function appNames(service: ServiceInfo, variables: readonly EnvironmentVariable[] | undefined): AppName[] {
  return (variables ?? [])
    .filter((v) => v.usedBy.some((u) => u.serviceId === service.id))
    .map((v) => ({ name: v.name, secret: v.secret }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

/** A name that's about a database, as DB_HOST, POSTGRES_PASSWORD or MYSQL_DATABASE. */
const ABOUT_A_DATABASE = /(^|_)(DB|DATABASE|POSTGRES|POSTGRESQL|PG|MYSQL|MARIADB|CLICKHOUSE|SQL)(_|$)/;

/**
 * The part a name of the app's suggests, as an entry would set it, or none for a
 * name that isn't about a database: SMTP_HOST is a mail server's.
 */
export function suggest(name: string): LinkPart {
  const n = name.toUpperCase();
  if (n === "DATABASE_URL") return LinkPart.URL;
  if (!ABOUT_A_DATABASE.test(n)) return LinkPart.UNSPECIFIED;
  if (/(^|_)(URL|URI|DSN)$/.test(n)) return LinkPart.URL;
  if (/(PASSWORD|PASS|PWD)$/.test(n)) return LinkPart.PASSWORD;
  if (/(^|_)(HOST|HOSTNAME|SERVER)$/.test(n)) return LinkPart.HOST;
  if (/^CLICKHOUSE_(NATIVE|TCP)_PORT$/.test(n)) return LinkPart.NATIVE_PORT;
  if (/(^|_)PORT$/.test(n)) return LinkPart.PORT;
  if (/(^|_)(USER|USERNAME)$/.test(n)) return LinkPart.USER;
  if (/(^|_)(NAME|DATABASE|DB)$/.test(n)) return LinkPart.DATABASE;
  return LinkPart.UNSPECIFIED;
}

/** What a part holds that only a secret may: the password, or a URL with it. */
export function secretPart(part: LinkPart): boolean {
  return part === LinkPart.PASSWORD || part === LinkPart.URL;
}

/** The parts a link to a database sets, in the order a person reads them. */
export const PARTS: readonly LinkPart[] = [
  LinkPart.HOST,
  LinkPart.PORT,
  LinkPart.NATIVE_PORT,
  LinkPart.ADDRESS,
  LinkPart.DATABASE,
  LinkPart.USER,
  LinkPart.PASSWORD,
  LinkPart.URL,
];

/** What each name is set to by default: what it suggests, where that fits a variable or a secret as it is. */
export function suggested(names: readonly AppName[]): Map<string, LinkPart> {
  const out = new Map<string, LinkPart>();
  let password = false;
  for (const n of names) {
    const part = suggest(n.name);
    if (part === LinkPart.UNSPECIFIED || (secretPart(part) && !n.secret)) continue;
    // One password is enough: the first a name suggests.
    if (part === LinkPart.PASSWORD) {
      if (password) continue;
      password = true;
    }
    out.set(n.name, part);
  }
  return out;
}

/** The secret an app's password is most likely in: one that suggests a password. */
export function passwordGuess(names: readonly AppName[]): string {
  return names.find((n) => n.secret && suggest(n.name) === LinkPart.PASSWORD)?.name ?? "";
}
