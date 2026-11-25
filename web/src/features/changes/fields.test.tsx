// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  ConnectionReadSchema,
  type FieldChange,
  FieldChangeSchema,
  FieldOp,
  HostAccessKind,
  HostAccessSchema,
  LinkPart,
  type LinkSet,
  LinkSetSchema,
  LinkStorageSchema,
  NeedStorageSchema,
} from "@dockwell/api";
import type { PlanChange, PlanValue } from "@dockwell/ui";
import { describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { renderWithApi } from "../../test/render";
import { type FieldSource, humanize, useFieldRows } from "./fields";

const added = (path: string, to: string, sensitive = false) =>
  create(FieldChangeSchema, { path, op: FieldOp.ADDED, to, sensitive });
const removed = (path: string, from: string) => create(FieldChangeSchema, { path, op: FieldOp.REMOVED, from });
const modified = (path: string, from: string, to: string, sensitive = false) =>
  create(FieldChangeSchema, { path, op: FieldOp.MODIFIED, from, to, sensitive });

function rowsOf(source: FieldSource): PlanChange[] {
  let rows: PlanChange[] = [];
  function Read() {
    rows = useFieldRows()(source);
    return null;
  }
  renderWithApi(<Read />, {});
  return rows;
}

/** A value as a person reads it. */
function read(value: PlanValue): string {
  if (typeof value === "object" && value !== null && "secret" in value) return "hidden";
  return renderWithApi(<span>{value as React.ReactNode}</span>, {}).container.textContent ?? "";
}

function reads(rows: readonly PlanChange[]): string[] {
  return rows.map((row) => {
    const parts = [row.label as string];
    if (row.from !== undefined) parts.push(`from ${read(row.from)}`);
    if (row.to !== undefined) parts.push(`to ${read(row.to)}`);
    if (row.hidden) parts.push("hidden");
    if (row.tone) parts.push(`[${row.tone}]`);
    return parts.join(" ");
  });
}

describe("a plan's fields, in words", () => {
  // The slice 3 review, M1: a set's row says its name and part though only one changed.
  it("says a set's name and what it reads though only one changed, and whose a URL's template is", () => {
    const uploads = create(LinkStorageSchema, { connection: "uploads", connectionLabel: "Uploads", prefix: "ghost/" });
    const set = (name: string, part: LinkPart, more: Partial<{ template: string; tlsOn: string }> = {}) =>
      create(LinkSetSchema, { name, part, ...more });
    const renamed = reads(
      rowsOf({
        kind: "link",
        changes: [modified("sets[0].name", "S3_BUCKET", "BUCKET_NAME")],
        storage: uploads,
        storageBefore: uploads,
        sets: [set("BUCKET_NAME", LinkPart.BUCKET)],
        setsBefore: [set("S3_BUCKET", LinkPart.BUCKET)],
      }),
    );
    expect(renamed).toEqual(["BUCKET_NAME to reads the bucket from Uploads, in place of S3_BUCKET [change]"]);
    const repart = reads(
      rowsOf({
        kind: "link",
        changes: [modified("sets[0].part", "bucket", "endpoint")],
        storage: uploads,
        storageBefore: uploads,
        sets: [set("S3_BUCKET", LinkPart.ENDPOINT)],
        setsBefore: [set("S3_BUCKET", LinkPart.BUCKET)],
      }),
    );
    expect(repart).toEqual(["S3_BUCKET to reads the endpoint from Uploads, in place of the bucket [change]"]);
    const url = reads(
      rowsOf({
        kind: "link",
        changes: [
          added("sets[0].name", "DATABASE_URL"),
          added("sets[0].part", "url"),
          added("sets[0].template", "postgres://{user}@{host}"),
          added("sets[0].tls_on", "require"),
        ],
        sets: [set("DATABASE_URL", LinkPart.URL, { template: "postgres://{user}@{host}", tlsOn: "require" })],
      }),
    );
    expect(url).toEqual([
      "DATABASE_URL to reads the URL",
      "DATABASE_URL's template to postgres://{user}@{host}",
      "DATABASE_URL with TLS to require",
    ]);
  });

  // The slice 3 re-check, N1: sets pair by name, so one added or removed beside others
  // is just that, and N2: a plan with no whole sets reads its fields as it did.
  it("pairs a link's sets by name, and leaves an older plan's sets as their fields", () => {
    const uploads = create(LinkStorageSchema, { connection: "uploads", connectionLabel: "Uploads", prefix: "ghost/" });
    const bucket = create(LinkSetSchema, { name: "S3_BUCKET", part: LinkPart.BUCKET });
    const endpoint = create(LinkSetSchema, { name: "S3_ENDPOINT", part: LinkPart.ENDPOINT });
    const region = create(LinkSetSchema, { name: "S3_REGION", part: LinkPart.REGION });
    const link = (changes: FieldChange[], sets: LinkSet[], setsBefore: LinkSet[]) =>
      reads(rowsOf({ kind: "link", changes, storage: uploads, storageBefore: uploads, sets, setsBefore }));
    expect(
      link(
        [
          modified("sets[1].name", "S3_REGION", "S3_ENDPOINT"),
          modified("sets[1].part", "region", "endpoint"),
          added("sets[2].name", "S3_REGION"),
          added("sets[2].part", "region"),
        ],
        [bucket, endpoint, region],
        [bucket, region],
      ),
    ).toEqual(["S3_ENDPOINT to reads the endpoint from Uploads"]);
    expect(
      link(
        [
          modified("sets[0].name", "S3_BUCKET", "S3_REGION"),
          modified("sets[0].part", "bucket", "region"),
          removed("sets[1].name", "S3_REGION"),
          removed("sets[1].part", "region"),
        ],
        [region],
        [bucket, region],
      ),
    ).toEqual(["S3_BUCKET from reads the bucket from Uploads"]);
    // A rename to another part, beside a set that stays, is one gone and one come.
    expect(
      link(
        [modified("sets[1].name", "S3_REGION", "S3_ENDPOINT"), modified("sets[1].part", "region", "endpoint")],
        [bucket, endpoint],
        [bucket, region],
      ),
    ).toEqual(["S3_ENDPOINT to reads the endpoint from Uploads", "S3_REGION from reads the region from Uploads"]);
    // The review's R1 and R2, on a plan from before sets were kept whole.
    expect(
      reads(
        rowsOf({
          kind: "link",
          changes: [modified("sets[0].name", "S3_BUCKET", "BUCKET_NAME")],
          storage: uploads,
          storageBefore: uploads,
        }),
      ),
    ).toEqual(["sets 1 name from S3_BUCKET to BUCKET_NAME"]);
    expect(
      reads(
        rowsOf({
          kind: "link",
          changes: [modified("sets[0].part", "bucket", "endpoint")],
          storage: uploads,
          storageBefore: uploads,
        }),
      ),
    ).toEqual(["sets 1 part from bucket to endpoint"]);
  });

  // The slice 3 review, L1: what a value reads says what differs.
  it("says which part a value read, where it reads another part of the same connection", () => {
    const read = (part: LinkPart, connection = "uploads", connectionLabel = "Uploads") =>
      create(ConnectionReadSchema, { connection, connectionLabel, part, link: "storage" });
    expect(
      reads(
        rowsOf({
          kind: "variable",
          changes: [modified("from.connection.part", "bucket", "endpoint")],
          reads: read(LinkPart.ENDPOINT),
          readsBefore: read(LinkPart.BUCKET),
        }),
      ),
    ).toEqual(["reads to the endpoint from Uploads, in place of the bucket [change]"]);
    expect(
      reads(
        rowsOf({
          kind: "variable",
          changes: [],
          reads: read(LinkPart.ENDPOINT, "files", "Ghost files"),
          readsBefore: read(LinkPart.BUCKET),
        }),
      ),
    ).toEqual(["reads to the endpoint from Ghost files, in place of the bucket from Uploads [change]"]);
  });

  // The slice 3 review, M2: two connections with one label are told apart by name.
  it("tells two connections with one label apart by name, where a value or a link moves between them", () => {
    const read = (connection: string) =>
      create(ConnectionReadSchema, { connection, connectionLabel: "Uploads", part: LinkPart.BUCKET, link: "storage" });
    expect(
      reads(
        rowsOf({
          kind: "variable",
          changes: [modified("from.connection.name", "uploads", "uploads-eu")],
          reads: read("uploads-eu"),
          readsBefore: read("uploads"),
        }),
      ),
    ).toEqual(["reads to the bucket from Uploads (uploads-eu), in place of Uploads (uploads) [change]"]);
    const kept = (connection: string) =>
      create(LinkStorageSchema, { connection, connectionLabel: "Uploads", prefix: "site/" });
    expect(
      reads(
        rowsOf({
          kind: "link",
          changes: [modified("connection", "uploads", "uploads-eu")],
          storage: kept("uploads-eu"),
          storageBefore: kept("uploads"),
        }),
      ),
    ).toEqual(["keeps its files on from Uploads (uploads), under site/ to Uploads (uploads-eu), under site/ [change]"]);
  });

  // The slice 3 review, L2: a catalog install's project says where a need comes from.
  it("says where a catalog install's need comes from, in place of its pin's fields", () => {
    const kept = (connection: string, connectionLabel: string) =>
      create(LinkStorageSchema, { connection, connectionLabel, prefix: "site/" });
    const moved = reads(
      rowsOf({
        kind: "project",
        changes: [modified("catalog.needs[storage].connection", "uploads", "files")],
        needs: [create(NeedStorageSchema, { need: "storage", storage: kept("files", "Ghost files") })],
        needsBefore: [create(NeedStorageSchema, { need: "storage", storage: kept("uploads", "Uploads") })],
      }),
    );
    expect(moved).toEqual(["storage comes from from Uploads, under site/ to Ghost files, under site/ [change]"]);
    const own = reads(
      rowsOf({
        kind: "project",
        changes: [removed("catalog.needs[storage].connection", "uploads"), added("catalog.needs[storage].own", "true")],
        needs: [create(NeedStorageSchema, { need: "storage", own: true })],
        needsBefore: [create(NeedStorageSchema, { need: "storage", storage: kept("uploads", "Uploads") })],
      }),
    );
    expect(own).toEqual(["storage comes from from Uploads, under site/ to its own values [change]"]);
  });

  // The slice 3 review, H1: a renamed connection is the same connection, by name.
  it("writes no row where a value reads the same connection under a new label, nor where a link keeps to it", () => {
    const read = (label: string) =>
      create(ConnectionReadSchema, {
        connection: "uploads",
        connectionLabel: label,
        part: LinkPart.BUCKET,
        link: "storage",
      });
    expect(
      reads(rowsOf({ kind: "variable", changes: [], reads: read("Media library"), readsBefore: read("Uploads") })),
    ).toEqual([]);
    const kept = (label: string) =>
      create(LinkStorageSchema, { connection: "uploads", connectionLabel: label, prefix: "ghost/" });
    expect(
      reads(rowsOf({ kind: "link", changes: [], storage: kept("Media library"), storageBefore: kept("Uploads") })),
    ).toEqual([]);
  });

  // ADR 0099, slice 3: each name a link sets, and a value read from a connection, in words.
  it("reads a link's sets as each name and what it reads, and a value as what it reads, in place of what it read", () => {
    const uploads = create(LinkStorageSchema, { connection: "uploads", connectionLabel: "Uploads", prefix: "ghost/" });
    const sets = reads(
      rowsOf({
        kind: "link",
        changes: [
          added("sets[0].name", "S3_BUCKET"),
          added("sets[0].part", "bucket"),
          added("sets[1].name", "S3_ACCESS_KEY_ID"),
          added("sets[1].part", "access-key-id"),
        ],
        storage: uploads,
        sets: [
          create(LinkSetSchema, { name: "S3_BUCKET", part: LinkPart.BUCKET }),
          create(LinkSetSchema, { name: "S3_ACCESS_KEY_ID", part: LinkPart.ACCESS_KEY_ID }),
        ],
      }),
    );
    expect(sets).toContain("S3_BUCKET to reads the bucket from Uploads");
    expect(sets).toContain("S3_ACCESS_KEY_ID to reads the access key ID from Uploads");
    expect(sets.some((row) => row.startsWith("sets"))).toBe(false);
    // A database's link reads as what each name reads, with no connection.
    expect(
      reads(
        rowsOf({
          kind: "link",
          changes: [added("sets[0].name", "DB_HOST"), added("sets[0].part", "host")],
          sets: [create(LinkSetSchema, { name: "DB_HOST", part: LinkPart.HOST })],
        }),
      ),
    ).toEqual(["DB_HOST to reads the host"]);
    const value = reads(
      rowsOf({
        kind: "variable",
        changes: [modified("from.connection.name", "files", "uploads")],
        reads: create(ConnectionReadSchema, {
          connection: "uploads",
          connectionLabel: "Uploads",
          part: LinkPart.BUCKET,
          link: "storage",
        }),
        readsBefore: create(ConnectionReadSchema, {
          connection: "files",
          connectionLabel: "Ghost files",
          part: LinkPart.BUCKET,
          link: "storage",
        }),
      }),
    );
    expect(value).toEqual(["reads to the bucket from Uploads, in place of Ghost files [change]"]);
  });

  // ADR 0099, slice 3: a link to a connection says where it keeps the app's files.
  it("says where a link keeps an app's files, by the connection's label, in place of its connection and prefix", () => {
    const uploads = create(LinkStorageSchema, { connection: "uploads", connectionLabel: "Uploads", prefix: "ghost/" });
    const media = create(LinkStorageSchema, {
      connection: "media",
      connectionLabel: "Media library",
      prefix: "ghost/",
    });
    const made = reads(
      rowsOf({
        kind: "link",
        changes: [added("label", "Storage"), added("connection", "uploads"), added("prefix", "ghost/")],
        storage: uploads,
      }),
    );
    expect(made).toContain("keeps its files on to Uploads, under ghost/");
    expect(made.some((row) => row.startsWith("connection") || row.startsWith("prefix"))).toBe(false);
    const moved = reads(
      rowsOf({
        kind: "link",
        changes: [modified("connection", "uploads", "media")],
        storage: media,
        storageBefore: uploads,
      }),
    );
    expect(moved).toEqual(["keeps its files on from Uploads, under ghost/ to Media library, under ghost/ [change]"]);
    const root = reads(
      rowsOf({
        kind: "link",
        changes: [removed("connection", "uploads")],
        storageBefore: create(LinkStorageSchema, { connection: "uploads", connectionLabel: "Uploads" }),
      }),
    );
    expect(root).toEqual(["keeps its files on from Uploads, at the bucket's root"]);
  });

  it("reads a branch by its own name, as people know it", () => {
    const rows = rowsOf({
      kind: "service",
      changes: [modified("build.git.ref", "refs/heads/main", "refs/tags/v1.4.2")],
    });
    expect(reads(rows)).toEqual(["branch from main to v1.4.2"]);
  });

  it("reads a route's sign-in in words, and what it has of its provider as one row (ADR 0087)", () => {
    const rows = rowsOf({
      kind: "route",
      changes: [
        added("domain", "sso.example.com"),
        added("access.sign_in.provider", "ours"),
        added("access.sign_in.emails", "mira@acme.dev"),
        added("access.sign_in.filled.issuer", "https://auth.example.com/"),
        added("access.sign_in.filled.client_id", "dockwell"),
        added("access.sign_in.filled.client_secret", "", true),
        added("access.sign_in.filled.revision", "1"),
      ],
    });
    const said = reads(rows);
    expect(said).toContain("signs in with to ours");
    expect(said).toContain("lets sign in to mira@acme.dev");
    expect(said.filter((r) => r.startsWith("its provider's settings"))).toHaveLength(1);
    expect(said.join(" | ")).not.toMatch(/filled|client|revision|auth\.example\.com/);
  });

  it("groups one port's fields into what they mean", () => {
    const rows = rowsOf({
      kind: "service",
      changes: [
        added("app.ports[0].host_port", "8080"),
        added("app.ports[0].container_port", "3000"),
        added("app.ports[0].protocol", "tcp"),
        added("app.ports[1].host_port", "53"),
        added("app.ports[1].container_port", "53"),
        added("app.ports[1].protocol", "udp"),
        added("app.ports[1].host_ip", "127.0.0.1"),
      ],
    });
    expect(reads(rows)).toEqual(["port to 8080 → 3000, TCP", "port to 127.0.0.1:53 → 53, UDP"]);
  });

  it("says what changed in a port that stayed, and a whole one that went", () => {
    const rows = rowsOf({
      kind: "service",
      changes: [
        modified("app.ports[0].host_port", "8080", "8081"),
        removed("app.ports[1].host_port", "9000"),
        removed("app.ports[1].container_port", "9000"),
        removed("app.ports[1].protocol", "tcp"),
      ],
    });
    expect(reads(rows)).toEqual(["port on the server from 8080 to 8081", "port from 9000 → 9000, TCP"]);
  });

  it("reads a volume and a server path as one row each, and a server path is a risk", () => {
    const rows = rowsOf({
      kind: "service",
      hostAccess: [
        create(HostAccessSchema, { kind: HostAccessKind.SERVER_PATH, path: "/srv/uploads" }),
        create(HostAccessSchema, { kind: HostAccessKind.DOCKER_SOCKET, path: "/var/run/docker.sock" }),
      ],
      changes: [
        added("app.image", "web:2"),
        added("app.mounts[0].volume", "web-data"),
        added("app.mounts[0].target", "/data"),
        added("app.mounts[1].host_path", "/srv/uploads"),
        added("app.mounts[1].target", "/uploads"),
        added("app.mounts[1].read_only", "true"),
        added("app.mounts[2].host_path", "/var/run/docker.sock"),
        added("app.mounts[2].target", "/var/run/docker.sock"),
      ],
    });
    // What reaches the server comes first, then what it runs.
    expect(reads(rows)).toEqual([
      "Docker's socket to /var/run/docker.sock [critical]",
      "server path to /srv/uploads at /uploads, read-only [warning]",
      "image to web:2",
      "volume to web-data at /data",
    ]);
  });

  it("calm when a server path goes: taking access away is no risk", () => {
    const rows = rowsOf({
      kind: "service",
      changes: [removed("app.mounts[0].host_path", "/srv/uploads"), removed("app.mounts[0].target", "/uploads")],
    });
    expect(reads(rows)).toEqual(["server path from /srv/uploads at /uploads"]);
  });

  it("reads a tmpfs as one row, with its size, and what changed in one that stayed", () => {
    const rows = rowsOf({
      kind: "service",
      changes: [
        added("app.shm_size", "1G"),
        added("app.tmpfs[0].target", "/run/cache"),
        added("app.tmpfs[0].size", "64M"),
        added("app.tmpfs[1].target", "/tmp"),
        modified("app.tmpfs[2].size", "64M", "128M"),
      ],
    });
    expect(reads(rows)).toEqual([
      "shared memory to 1G",
      "tmpfs to /run/cache, up to 64M",
      "tmpfs to /tmp",
      "tmpfs size from 64M to 128M",
    ]);
  });

  it("names a variable and hides what it holds, however it changed", () => {
    const rows = rowsOf({
      kind: "service",
      changes: [
        added("app.env[DATABASE_URL]", "", true),
        modified("app.env[API_KEY]", "", "", true),
        create(FieldChangeSchema, { path: "app.env[OLD]", op: FieldOp.REMOVED, sensitive: true }),
      ],
    });
    expect(reads(rows)).toEqual([
      "variable to DATABASE_URL hidden",
      "variable to API_KEY hidden [change]",
      "variable from OLD hidden",
    ]);
  });

  it("reads a volume's and a network's fields, and marks what reaches past Docker", () => {
    expect(
      reads(
        rowsOf({
          kind: "volume",
          hostAccess: [create(HostAccessSchema, { kind: HostAccessKind.VOLUME_MOUNT, type: "nfs" })],
          changes: [added("driver", "local"), added("options[device]", "", true)],
        }),
      ),
    ).toEqual(["driver to local [warning]", "option to device hidden [warning]"]);
    expect(reads(rowsOf({ kind: "network", changes: [added("driver", "bridge"), added("internal", "true")] }))).toEqual(
      ["driver to bridge", "internal to yes"],
    );
  });

  it("reads a route and a firewall rule whole, on one row", () => {
    expect(
      reads(
        rowsOf({
          kind: "route",
          changes: [
            added("domain", "app.example.com"),
            added("path", "/api"),
            added("service", "web"),
            added("port", "8080"),
          ],
        }),
      ),
    ).toEqual(["route to app.example.com/api → web:8080"]);
    expect(reads(rowsOf({ kind: "route", changes: [modified("port", "8080", "9090")] }))).toEqual([
      "port from 8080 to 9090",
    ]);
    expect(
      reads(
        rowsOf({
          kind: "firewall.rule",
          changes: [
            added("action", "allow"),
            added("protocol", "tcp"),
            added("port", "443"),
            added("sources", "10.0.0.0/8"),
          ],
        }),
      ),
    ).toEqual(["rule to allow 443, TCP, from 10.0.0.0/8"]);
    expect(reads(rowsOf({ kind: "firewall.rule", changes: [added("action", "allow"), added("port", "22")] }))).toEqual([
      "rule to allow 22, TCP, from anywhere",
    ]);
    expect(
      reads(rowsOf({ kind: "firewall.rule", changes: [added("action", "deny"), added("sources", "2001:db8::/32")] })),
    ).toEqual(["rule to block all ports from 2001:db8::/32"]);
  });

  it("reads a kind no one has words for yet as words, from its fields' names", () => {
    const rows = rowsOf({
      kind: "ssl.certificate",
      changes: [
        added("not_after", "2027-01-01"),
        modified("spec.max_body_size", "1", "2"),
        added("backends[0].host", "a"),
        added("labels[team]", "ops"),
        added("force_https", "true"),
        added("secret_key", "", true),
      ],
    });
    expect(reads(rows)).toEqual([
      "not after to 2027-01-01",
      "spec max body size from 1 to 2",
      "backends 1 host to a",
      "label to team=ops",
      "force https to yes",
      "secret key to hidden",
    ]);
    expect(humanize("limits.cpu[2].max_burst")).toBe("limits cpu 3 max burst");
  });

  it("writes a name someone gave as they wrote it, underscores and all", () => {
    expect(humanize("variables[FEATURE_FLAGS]")).toBe("variables FEATURE_FLAGS");
    expect(humanize("secrets[DB_PASSWORD].from_env")).toBe("secrets DB_PASSWORD from env");
    const rows = rowsOf({
      kind: "service",
      changes: [
        create(FieldChangeSchema, { path: "variables[FEATURE_FLAGS]", op: FieldOp.ADDED, to: "FEATURE_FLAGS" }),
        create(FieldChangeSchema, {
          path: "secrets[WORDPRESS_DB_PASSWORD]",
          op: FieldOp.ADDED,
          to: "MARIADB_PASSWORD",
        }),
      ],
    });
    // A shared value's name is the value, and the name the app has it by follows where it differs,
    // so a long name never makes the field column wide for every row.
    expect(reads(rows)).toEqual([
      "uses variable to FEATURE_FLAGS",
      "uses secret to MARIADB_PASSWORD as WORDPRESS_DB_PASSWORD",
    ]);
  });

  it("says a value it uses moved on, not the revision it's pinned to", () => {
    const rows = rowsOf({
      kind: "service",
      changes: [
        create(FieldChangeSchema, { path: "revisions[SMTP_HOST]", op: FieldOp.MODIFIED, from: "1", to: "2" }),
        // One that comes with a value it starts to use says nothing that value's own row doesn't.
        create(FieldChangeSchema, { path: "revisions[NEW_ONE]", op: FieldOp.ADDED, to: "1" }),
        create(FieldChangeSchema, { path: "variables[NEW_ONE]", op: FieldOp.ADDED, to: "NEW_ONE" }),
      ],
    });
    expect(reads(rows)).toEqual(["SMTP_HOST to uses its newer value [change]", "uses variable to NEW_ONE"]);
  });

  it("names a database user and what it's given in words", () => {
    let word: string | undefined;
    function Read() {
      word = useFieldRows().kindWord?.("database.user");
      return null;
    }
    renderWithApi(<Read />, {});
    expect(word).toBe("database user");
    const rows = rowsOf({
      kind: "database.user",
      changes: [added("database_name", "orders"), added("password", "ORDERS_DATABASE_PASSWORD")],
    });
    expect(reads(rows)).toEqual(["database to orders", "password in to ORDERS_DATABASE_PASSWORD"]);
  });

  it("reads in Chinese", async () => {
    await activateLocale("zh-CN");
    const rows = rowsOf({
      kind: "service",
      changes: [added("app.mounts[0].volume", "web-data"), added("app.mounts[0].target", "/data")],
    });
    expect(reads(rows)).toEqual(["卷 to web-data 挂载到 /data"]);
  });
});

describe("a route published at Cloudflare (ADR 0104)", () => {
  it("reads in words, the tunnel by its name, with none of its fields or IDs", async () => {
    await activateLocale("en");
    const rows = rowsOf({
      kind: "route",
      tunnel: "web-1",
      changes: [
        added("domain", "wiki.example.com"),
        added("service", "wiki-app"),
        added("port", "80"),
        added("tls", "off"),
        added("trusted_proxies", "127.0.0.1/32"),
        added("publish.way", "server-tunnel"),
        added("publish.tunnel", "3f2d1c0b-4a5b-4c6d-8e7f-9a0b1c2d3e4f"),
        added("publish.provider", "cloudflare"),
      ],
    });
    const said = rows.map((r) => `${String(r.label)}: ${String(r.to ?? "")}`);
    expect(said).toContain("HTTPS: Ends at Cloudflare");
    expect(said).toContain("through: the tunnel web-1");
    expect(said.join(" ")).not.toMatch(/3f2d1c0b|publish|127\.0\.0\.1|tls/);
  });
});
