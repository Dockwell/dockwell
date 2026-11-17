// SPDX-License-Identifier: AGPL-3.0-only

import { type ConnectionInfo, LinkPart } from "@dockwell/api";

import type { AppName } from "../apps/connectDatabase";

/** The parts a link to S3 storage sets, in the order a person reads them. */
export const STORAGE_PARTS: readonly LinkPart[] = [
  LinkPart.ENDPOINT,
  LinkPart.REGION,
  LinkPart.BUCKET,
  LinkPart.ACCESS_KEY_ID,
  LinkPart.SECRET_KEY,
  LinkPart.PUBLIC_URL,
  LinkPart.PATH_STYLE,
  LinkPart.KEY_PREFIX,
];

/** Who a link serves: each app that reads a name it sets, or its environment, where none does yet, as the server counts them. */
export function servedBy(use: { environment: string; appIds: readonly string[] }): string[] {
  return use.appIds.length > 0 ? [...use.appIds] : [use.environment];
}

/**
 * How many apps use a connection: an app is a service, each reading names a link sets
 * (ADR 0084), and links no service reads yet are one app of their environment, where
 * no service there reads one, as the server counts them.
 */
export function appsOn(connection: { uses: readonly { environment: string; appIds: readonly string[] }[] }): number {
  const services = new Set(connection.uses.flatMap((u) => u.appIds));
  const read = new Set(connection.uses.filter((u) => u.appIds.length > 0).map((u) => u.environment));
  const unread = new Set(
    connection.uses.filter((u) => u.appIds.length === 0 && !read.has(u.environment)).map((u) => u.environment),
  );
  return services.size + unread.size;
}

/**
 * Where a link's app keeps its files in the bucket: its prefix, where the link sets
 * the name that tells the app it, and otherwise the bucket's root, "", as the server
 * reads it (ADR 0099).
 */
export function keptPrefix(use: { prefix: string; sets: Record<string, LinkPart> }): string {
  return Object.values(use.sets).includes(LinkPart.KEY_PREFIX) ? use.prefix : "";
}

/** What only a secret holds: the keys, as plans show a variable. */
export function keyPart(part: LinkPart): boolean {
  return part === LinkPart.ACCESS_KEY_ID || part === LinkPart.SECRET_KEY;
}

/** A name that's about S3 storage: S3_BUCKET, AWS_ACCESS_KEY_ID, STORAGE_ENDPOINT, MINIO_ROOT_USER. */
const ABOUT_STORAGE = /(^|_)(S3|AWS|STORAGE|BUCKET|MINIO|R2|B2|GCS|OBJECT|FILE_STORAGE|UPLOADS?)(_|$)/;

/**
 * The part a name of the app's suggests, as an entry would set it, or none for a
 * name that isn't about storage, or that no part holds whole.
 */
export function suggestStoragePart(name: string): LinkPart {
  const n = name.toUpperCase();
  if (!ABOUT_STORAGE.test(n)) return LinkPart.UNSPECIFIED;
  if (/(SECRET_ACCESS_KEY|SECRET_KEY|ACCESS_SECRET|SECRET)$/.test(n)) return LinkPart.SECRET_KEY;
  if (/(ACCESS_KEY_ID|ACCESS_KEY|KEY_ID)$/.test(n)) return LinkPart.ACCESS_KEY_ID;
  if (/(FORCE_PATH_STYLE|PATH_STYLE|USE_PATH_STYLE)$/.test(n)) return LinkPart.PATH_STYLE;
  if (/(KEY_PREFIX|PATH_PREFIX|PREFIX)$/.test(n)) return LinkPart.KEY_PREFIX;
  if (/(PUBLIC_URL|PUBLIC_ENDPOINT|CDN_URL|CUSTOM_DOMAIN)$/.test(n)) return LinkPart.PUBLIC_URL;
  if (/(ENDPOINT|ENDPOINT_URL|URL|HOST)$/.test(n)) return LinkPart.ENDPOINT;
  if (/REGION$/.test(n)) return LinkPart.REGION;
  if (/(BUCKET|BUCKET_NAME)$/.test(n)) return LinkPart.BUCKET;
  return LinkPart.UNSPECIFIED;
}

/** What each name is set to by default: what it suggests, where that fits a variable or a secret as it is, each part once. */
export function suggestedStorage(names: readonly AppName[]): Map<string, LinkPart> {
  const out = new Map<string, LinkPart>();
  const taken = new Set<LinkPart>();
  for (const n of names) {
    const part = suggestStoragePart(n.name);
    if (part === LinkPart.UNSPECIFIED || taken.has(part) || (keyPart(part) && !n.secret)) continue;
    taken.add(part);
    out.set(n.name, part);
  }
  return out;
}

/** A link's name in an environment, not yet taken: storage, then storage-2. */
export function freeLinkName(taken: readonly string[]): string {
  if (!taken.includes("storage")) return "storage";
  for (let i = 2; ; i++) if (!taken.includes(`storage-${i}`)) return `storage-${i}`;
}

/** Two prefixes in one bucket overlap: the same, or one holds the other, as "" holds all. */
export function prefixesMeet(a: string, b: string): boolean {
  return a.startsWith(b) || b.startsWith(a);
}

/**
 * The first of want ("outline/"), "outline-2/", "outline-3/" and on that meets no
 * prefix taken on the connection, as the server picks it. The root, or a connection
 * with a link at its root, has no other to offer.
 */
export function freePrefix(want: string, taken: readonly string[]): string {
  const free = (p: string) => !taken.some((t) => prefixesMeet(p, t));
  if (free(want) || want === "" || taken.includes("")) return want;
  // Within a prefix that's taken no sibling is free, so start from the first folder.
  const stem = taken.some((t) => want.startsWith(t)) ? want.split("/")[0] : want.replace(/\/$/, "");
  for (let i = 2; ; i++) if (free(`${stem}-${i}/`)) return `${stem}-${i}/`;
}

/** The links of an environment there are, from what reads each connection. */
export function linksIn(connections: readonly ConnectionInfo[], environment: string): string[] {
  return connections.flatMap((c) => c.uses.filter((u) => u.environment === environment).map((u) => u.link));
}

/** An app that reads a connection, and the environment it's in. */
export interface ReadingApp {
  id: string;
  name: string;
  environment: string;
}

/**
 * The apps that read a connection, each once, by ID: an app of the same name in
 * another environment is another app.
 */
export function appsOf(connection: ConnectionInfo): ReadingApp[] {
  const out = new Map<string, ReadingApp>();
  for (const u of connection.uses) {
    u.appIds.forEach((id, i) => {
      if (!out.has(id)) out.set(id, { id, name: u.apps[i] ?? id, environment: u.environment });
    });
  }
  return [...out.values()];
}
