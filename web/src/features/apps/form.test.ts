// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { ImageConfigSchema, Protocol } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { appEdits, fieldOf, fillFromImage, type ImageForm, parseEnv, pathReach, validPort, volumeName } from "./form";

const empty: ImageForm = { image: "nginx:1.27", name: "", ports: [], env: [], mounts: [] };
const scope = { environment: "", servers: [], volumes: [] };

describe("New app's edits", () => {
  it("are a service alone for an image alone", () => {
    const edits = appEdits(empty, "nginx", scope);
    expect(edits).toHaveLength(1);
    expect(edits[0]?.resource).toEqual({ kind: "service", scope: "", name: "nginx" });
  });

  it("make the volumes that don't exist yet, once each, before the service", () => {
    const form: ImageForm = {
      ...empty,
      mounts: [
        { key: 1, source: "data", target: "/data", readOnly: false },
        { key: 2, source: "data", target: "/also", readOnly: true },
        { key: 3, source: "known", target: "/known", readOnly: false },
        { key: 4, source: "/srv/conf", target: "/conf", readOnly: true },
      ],
    };
    const edits = appEdits(form, "web", { environment: "env_1", servers: ["srv_1"], volumes: ["known"] });
    expect(edits.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual(["volume data", "service web"]);
    expect(edits[0]?.servers).toEqual(["srv_1"]);
    expect(edits[0]?.resource?.scope).toBe("env_1");
  });

  it("carry ports, variables and mounts as the spec has them", () => {
    const form: ImageForm = {
      ...empty,
      ports: [{ key: 1, host: "8080", container: "80", protocol: "udp" }],
      env: [{ key: 2, name: " LOG_LEVEL ", value: "info" }],
      mounts: [{ key: 3, source: "/srv/conf", target: "/conf", readOnly: true }],
    };
    const spec = appEdits(form, "web", scope)[0]?.spec?.kind;
    expect(spec?.case).toBe("service");
    const app = spec?.case === "service" ? spec.value.runs : undefined;
    expect(app?.case).toBe("app");
    const value = app?.case === "app" ? app.value : undefined;
    expect(value?.ports).toEqual([{ hostPort: 8080, containerPort: 80, protocol: Protocol.UDP }]);
    expect(value?.env).toEqual({ LOG_LEVEL: "info" });
    expect(value?.mounts).toEqual([
      { source: { case: "hostPath", value: "/srv/conf" }, target: "/conf", readOnly: true },
    ]);
  });
});

describe("what a server path reaches", () => {
  it("tells Docker's socket from any other path, and a volume from both", () => {
    expect(pathReach("/var/run/docker.sock")).toBe("socket");
    expect(pathReach("/run/containerd/containerd.sock")).toBe("socket");
    expect(pathReach("/var/run/")).toBe("socket");
    expect(pathReach("/srv/data")).toBe("path");
    expect(pathReach("data")).toBeUndefined();
    expect(pathReach("")).toBeUndefined();
  });
});

describe("pasting an .env file", () => {
  it("reads KEY=value a line, leaving out comments and quotes", () => {
    expect(parseEnv("A=1\n# a comment\n\nexport B=\"two words\"\nC='3'\nD=x=y")).toEqual([
      { name: "A", value: "1" },
      { name: "B", value: "two words" },
      { name: "C", value: "3" },
      { name: "D", value: "x=y" },
    ]);
  });

  it("leaves what isn't one for the field to take as it is", () => {
    expect(parseEnv("not a variable")).toBeUndefined();
    expect(parseEnv("A B=1")).toBeUndefined();
    expect(parseEnv("")).toBeUndefined();
  });
});

describe("a port", () => {
  it("is a whole number from 1 to 65535", () => {
    expect(validPort("80")).toBe(true);
    expect(validPort("0")).toBe(false);
    expect(validPort("65536")).toBe(false);
    expect(validPort("8e3")).toBe(false);
    expect(validPort("")).toBe(false);
  });
});

describe("where a refusal belongs", () => {
  const form: ImageForm = {
    image: "bad image",
    name: "web",
    ports: [{ key: 1, host: "8080", container: "80", protocol: "tcp" }],
    env: [],
    mounts: [{ key: 2, source: "/etc", target: "/host", readOnly: false }],
  };

  it("is the field whose value it names", () => {
    expect(fieldOf("The server path /etc mounted at /host is refused", form, "web")).toBe("mounts.2.source");
    expect(fieldOf("Port 8080 is already published", form, "web")).toBe("ports.1.host");
    expect(fieldOf('The image "bad image" has a space', { ...form, mounts: [], ports: [] }, "web")).toBe("image");
    expect(fieldOf("Its name is taken", { ...form, mounts: [], ports: [] }, "web")).toBe("name");
  });

  it("is none of them when it names none", () => {
    expect(fieldOf("hz-1 can't run it now: Docker isn't running", { ...form, mounts: [], ports: [] }, "web")).toBe("");
  });
});

describe("filling New app from an image", () => {
  let n = 0;
  const key = () => ++n;
  const empty: ImageForm = { image: "postgres:17", name: "", ports: [], env: [], mounts: [] };
  const postgres = create(ImageConfigSchema, {
    ports: [{ port: 5432 }],
    env: [
      { name: "PATH", value: "/usr/bin" },
      { name: "PG_MAJOR", value: "17" },
      { name: "PGDATA", value: "/var/lib/postgresql/data" },
    ],
    volumes: ["/var/lib/postgresql/data"],
  });

  it("takes the port it exposes, a volume for what it keeps, and the variables someone may set", () => {
    const filled = fillFromImage(empty, postgres, "postgres", key);
    expect(filled.ports).toMatchObject([{ container: "5432", auto: true, fromImage: true, protocol: "tcp" }]);
    expect(filled.mounts).toMatchObject([
      { source: "postgres-data", target: "/var/lib/postgresql/data", fromImage: true },
    ]);
    expect(filled.env.map((e) => e.name)).toEqual(["PGDATA"]);
    // Left as the image gives it, the spec needn't say it.
    const edits = appEdits(filled, "postgres", { environment: "", servers: [], volumes: [] });
    const app = edits.at(-1)?.spec?.kind?.case === "service" ? edits.at(-1)?.spec?.kind?.value : undefined;
    expect(app?.runs?.case === "app" ? app.runs.value?.env : undefined).toEqual({});
  });

  it("refills only what nobody changed when the image changes", () => {
    const filled = fillFromImage(empty, postgres, "postgres", key);
    const mine = {
      ...filled,
      env: filled.env.map((e) => ({ ...e, value: "/srv/pg", fromImage: false })),
      mounts: [...filled.mounts, { key: key(), source: "/srv/backups", target: "/backups", readOnly: true }],
    };
    const redis = create(ImageConfigSchema, { ports: [{ port: 6379 }], volumes: ["/data"] });
    const again = fillFromImage(mine, redis, "redis", key);
    expect(again.ports).toMatchObject([{ container: "6379", fromImage: true }]);
    expect(again.env).toMatchObject([{ name: "PGDATA", value: "/srv/pg" }]);
    expect(again.mounts.map((m) => `${m.source}:${m.target}`)).toEqual(["/srv/backups:/backups", "redis-data:/data"]);
    // A port someone added stays, and the image adds none beside it.
    const withPort = { ...again, ports: [{ key: key(), host: "9000", container: "9000", protocol: "tcp" as const }] };
    expect(fillFromImage(withPort, postgres, "postgres", key).ports).toMatchObject([{ host: "9000" }]);
  });

  it("names a volume for its path", () => {
    expect(volumeName("ghost", "/var/lib/ghost/content")).toBe("ghost-content");
    expect(volumeName("app", "/")).toBe("app-data");
    expect(volumeName("web", "/Var/Cache_Dir/")).toBe("web-cache-dir");
  });
});
