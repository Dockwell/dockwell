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
];

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

/** The links of an environment there are, from what reads each connection. */
export function linksIn(connections: readonly ConnectionInfo[], environment: string): string[] {
  return connections.flatMap((c) => c.uses.filter((u) => u.environment === environment).map((u) => u.link));
}

/** The apps that read a connection, each once, by name. */
export function appsOf(connection: ConnectionInfo): string[] {
  return [...new Set(connection.uses.flatMap((u) => u.apps))];
}
