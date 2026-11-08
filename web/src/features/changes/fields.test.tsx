// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { FieldChangeSchema, FieldOp, HostAccessKind, HostAccessSchema } from "@dockwell/api";
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
  it("reads a branch by its own name, as people know it", () => {
    const rows = rowsOf({
      kind: "service",
      changes: [modified("build.git.ref", "refs/heads/main", "refs/tags/v1.4.2")],
    });
    expect(reads(rows)).toEqual(["branch from main to v1.4.2"]);
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
      ],
    });
    expect(reads(rows)).toEqual(["variables FEATURE_FLAGS to FEATURE_FLAGS"]);
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
    expect(reads(rows)).toEqual(["SMTP_HOST to uses its newer value [change]", "variables NEW_ONE to NEW_ONE"]);
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
