// SPDX-License-Identifier: AGPL-3.0-only

/**
 * The S3-compatible stores a connection is made for, each with what it takes to
 * reach a bucket there: its endpoint, made from an account ID or a region where the
 * store has one, and whether its bucket goes in the path. A person picks the store
 * and types only what the store can't say (ADR 0099).
 */
export type ProviderId = "r2" | "s3" | "b2" | "gcs" | "other";

export interface Provider {
  id: ProviderId;
  /** The store's name, which no translation changes. */
  name: string;
  /** Two letters for its tile, as the mocks have them. */
  mark: string;
  /** Its regions, where a person picks one, first the default. Empty where the store has none. */
  regions: readonly string[];
  /** Its account ID makes its endpoint, as Cloudflare R2's does. */
  account: boolean;
  /** Whether a bucket goes in the path, rather than the host, by default. */
  pathStyle: boolean;
  /** Its endpoint, from the account ID or region typed; empty where it needs one not yet typed. */
  endpoint: (place: { account: string; region: string }) => string;
  /** The region a connection names, for the store's own signing: R2 and GCS take auto. */
  region: (place: { region: string }) => string;
}

const AWS_REGIONS = [
  "us-east-1",
  "us-east-2",
  "us-west-1",
  "us-west-2",
  "ca-central-1",
  "eu-west-1",
  "eu-west-2",
  "eu-west-3",
  "eu-central-1",
  "eu-north-1",
  "ap-south-1",
  "ap-southeast-1",
  "ap-southeast-2",
  "ap-northeast-1",
  "sa-east-1",
] as const;

const B2_REGIONS = ["us-west-004", "us-west-002", "us-west-001", "us-east-005", "eu-central-003"] as const;

/** A Cloudflare account ID: 32 hex digits. */
export const R2_ACCOUNT = /^[0-9a-f]{32}$/;

export const PROVIDERS: readonly Provider[] = [
  {
    id: "r2",
    name: "Cloudflare R2",
    mark: "R2",
    regions: [],
    account: true,
    pathStyle: true,
    endpoint: ({ account }) =>
      R2_ACCOUNT.test(account.trim()) ? `https://${account.trim()}.r2.cloudflarestorage.com` : "",
    region: () => "auto",
  },
  {
    id: "s3",
    name: "Amazon S3",
    mark: "S3",
    regions: AWS_REGIONS,
    account: false,
    pathStyle: false,
    endpoint: ({ region }) => (region ? `https://s3.${region}.amazonaws.com` : ""),
    region: ({ region }) => region,
  },
  {
    id: "b2",
    name: "Backblaze B2",
    mark: "B2",
    regions: B2_REGIONS,
    account: false,
    pathStyle: false,
    endpoint: ({ region }) => (region ? `https://s3.${region}.backblazeb2.com` : ""),
    region: ({ region }) => region,
  },
  {
    id: "gcs",
    name: "Google Cloud Storage",
    mark: "GC",
    regions: [],
    account: false,
    pathStyle: true,
    endpoint: () => "https://storage.googleapis.com",
    region: () => "auto",
  },
  {
    id: "other",
    name: "",
    mark: "+",
    regions: [],
    account: false,
    pathStyle: true,
    endpoint: () => "",
    region: ({ region }) => region || "auto",
  },
];

export function provider(id: ProviderId): Provider {
  return PROVIDERS.find((p) => p.id === id) ?? (PROVIDERS[PROVIDERS.length - 1] as Provider);
}

/** Where an endpoint is: the store it names, and the account ID or region in it. */
export interface Place {
  provider: ProviderId;
  account: string;
  region: string;
}

/** The store an endpoint is at, as a saved connection names it, with what made it. */
export function placeOf(endpoint: string, region = ""): Place {
  let host = "";
  try {
    host = new URL(endpoint).host.toLowerCase();
  } catch {
    return { provider: "other", account: "", region };
  }
  const r2 = /^([0-9a-f]{32})\.r2\.cloudflarestorage\.com$/.exec(host);
  if (r2) return { provider: "r2", account: r2[1] ?? "", region: "" };
  const aws = /^s3[.-]([a-z0-9-]+)\.amazonaws\.com$/.exec(host);
  if (aws) return { provider: "s3", account: "", region: aws[1] ?? region };
  const b2 = /^s3\.([a-z0-9-]+)\.backblazeb2\.com$/.exec(host);
  if (b2) return { provider: "b2", account: "", region: b2[1] ?? region };
  if (host === "storage.googleapis.com") return { provider: "gcs", account: "", region: "" };
  return { provider: "other", account: "", region };
}

/** The endpoint's host, as a list shows where a connection is. */
export function hostOf(endpoint: string): string {
  try {
    return new URL(endpoint).host;
  } catch {
    return endpoint;
  }
}

/** A bucket's name as every S3 service takes it, as the server checks it. */
export const BUCKET = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/;

/** A connection's name, from its bucket, not yet taken: lowercase, digits and dashes. */
export function freeConnectionName(base: string, taken: readonly string[]): string {
  const clean =
    base
      .toLowerCase()
      .replace(/[^a-z0-9-]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 40) || "storage";
  const start = /^[a-z]/.test(clean) ? clean : `s-${clean}`;
  if (!taken.includes(start)) return start;
  for (let i = 2; ; i++) if (!taken.includes(`${start}-${i}`)) return `${start}-${i}`;
}
