// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import type { DescService } from "@bufbuild/protobuf";
import * as api from "@dockwell/api";
import { Protocol, RestartPolicy, ServiceService, SpecSchema, WebsiteService } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { editCommand, flagName, kindNoun, methodCommand, methodFlags, methodName, shellWord, specFlags } from "./cli";

// Vitest runs in the panel's package, web/.
const generated = readFileSync(resolve("../internal/cli/apicmd/apicmd.gen.go"), "utf8");

interface GoField {
  path: string;
  flag: string | undefined;
  arg: boolean;
  sensitive: boolean;
  fromFile: boolean;
}

function goFields(block: string): GoField[] {
  const out: GoField[] = [];
  for (const line of block.split("\n")) {
    const m = /^\s*\{Path: "([^"]+)"(?:, Flag: "([^"]+)")?/.exec(line);
    if (!m) continue;
    out.push({
      path: m[1] ?? "",
      flag: m[2],
      arg: /, Arg: true[,}]/.test(line),
      sensitive: /, Sensitive: true[,}]/.test(line),
      fromFile: /, FromFile: true[,}]/.test(line),
    });
  }
  return out;
}

/** The generated table's methods, by full name: their noun, verb and flags. */
function goMethods(): Map<string, { noun: string; verb: string; fields: GoField[] }> {
  const out = new Map<string, { noun: string; verb: string; fields: GoField[] }>();
  for (const block of generated.split(/\n\t\t\{\n\t\t\tName: +/).slice(1)) {
    const name = /^"([^"]+)"/.exec(block)?.[1] ?? "";
    const noun = /\n\t\t\tNoun: +"([^"]+)"/.exec(block)?.[1] ?? "";
    const verb = /\n\t\t\tVerb: +"([^"]+)"/.exec(block)?.[1] ?? "";
    const fields = goFields(block.split("\n\t\t\tCall:")[0] ?? "");
    out.set(name, { noun, verb, fields });
  }
  return out;
}

/** The generated table's kinds, by the field of the spec that holds each. */
function goKinds(): Map<string, { noun: string; fields: GoField[] }> {
  const out = new Map<string, { noun: string; fields: GoField[] }>();
  for (const block of generated.split(/\n\t\t\{\n\t\t\tField: +/).slice(1)) {
    const field = /^"([^"]+)"/.exec(block)?.[1] ?? "";
    const noun = /\n\t\t\tNoun: +"([^"]+)"/.exec(block)?.[1] ?? "";
    out.set(field, { noun, fields: goFields(block.split("\n\t\t},")[0] ?? "") });
  }
  return out;
}

const services = (Object.values(api) as unknown[]).filter(
  (v): v is DescService =>
    typeof v === "object" && v !== null && (v as { kind?: string }).kind === "service" && "methods" in v,
);

describe("the CLI's names, as the panel writes them", () => {
  it("name every method's command as the generated CLI does", () => {
    const table = goMethods();
    expect(table.size).toBeGreaterThan(100);
    let compared = 0;
    for (const service of services) {
      for (const method of service.methods) {
        const want = table.get(`${service.typeName}.${method.name}`);
        expect(want, `${service.typeName}.${method.name} is in the generated table`).toBeDefined();
        if (!want) continue;
        expect(methodName(method), `${service.typeName}.${method.name}`).toEqual({ noun: want.noun, verb: want.verb });
        const flags = methodFlags(method);
        expect(
          flags.map((f) => ({
            path: f.path,
            flag: f.sensitive && f.flag.endsWith("-file") ? f.flag.slice(0, -5) : f.flag,
            arg: f.arg,
            sensitive: f.sensitive,
            fromFile: f.fromFile,
          })),
          `${service.typeName}.${method.name}'s flags`,
        ).toEqual(
          want.fields.map((f) => ({
            path: f.path,
            flag: f.flag ?? flagName(f.path),
            arg: f.arg,
            sensitive: f.sensitive,
            fromFile: f.fromFile,
          })),
        );
        compared++;
      }
    }
    expect(compared).toBe(table.size);
  });

  it("name every kind's commands and flags as the generated CLI does", () => {
    const table = goKinds();
    const kinds = SpecSchema.oneofs.find((o) => o.name === "kind")?.fields ?? [];
    expect(kinds.length).toBeGreaterThan(5);
    expect(kinds.map((k) => k.name).sort()).toEqual([...table.keys()].sort());
    for (const kind of kinds) {
      const want = table.get(kind.name);
      expect(kindNoun(kind.name)).toBe(want?.noun);
      if (kind.fieldKind !== "message") throw new Error(`${kind.name} isn't a message`);
      expect(
        specFlags(kind.message).map((f) => ({
          path: f.path,
          flag: f.sensitive && f.flag.endsWith("-file") ? f.flag.slice(0, -5) : f.flag,
          sensitive: f.sensitive,
        })),
        kind.name,
      ).toEqual(
        (want?.fields ?? []).map((f) => ({ path: f.path, flag: f.flag ?? flagName(f.path), sensitive: f.sensitive })),
      );
    }
  });
});

describe("shellWord", () => {
  it("leaves a plain word as it is, and quotes the rest so a shell reads them whole", () => {
    expect(shellWord("nginx:1.27")).toBe("nginx:1.27");
    expect(shellWord("/usr/share/nginx/html")).toBe("/usr/share/nginx/html");
    expect(shellWord("NAME=value")).toBe("NAME=value");
    expect(shellWord("")).toBe("''");
    expect(shellWord("two words")).toBe("'two words'");
    expect(shellWord(`{"a":1}`)).toBe(`'{"a":1}'`);
    expect(shellWord("it's $HOME")).toBe(`'it'\\''s $HOME'`);
  });
});

describe("editCommand", () => {
  const app = {
    resource: { kind: "service", scope: "env_1", name: "shop" },
    spec: {
      kind: {
        case: "service" as const,
        value: {
          runs: {
            case: "app" as const,
            value: {
              image: "nginx:1.27",
              env: { NGINX_HOST: "shop.example.com", API_KEY: "s3cret" },
              ports: [{ hostPort: 8083, containerPort: 80, protocol: Protocol.TCP }],
              mounts: [{ source: { case: "volume" as const, value: "site" }, target: "/usr/share/nginx/html" }],
              privileged: false,
            },
          },
        },
      },
    },
  };

  it("writes a service as services create, with each value a flag, and secrets left to the environment", () => {
    expect(editCommand("create", app, {}, (path, key) => path === "app.env" && key === "NGINX_HOST")).toBe(
      "dockwell services create shop --app-image nginx:1.27 --app-env NGINX_HOST=shop.example.com --app-env API_KEY" +
        ` --app-ports '{"hostPort":8083,"containerPort":80,"protocol":"PROTOCOL_TCP"}'` +
        ` --app-mounts '{"volume":"site","target":"/usr/share/nginx/html"}'`,
    );
  });

  it("says where it goes only where there's a choice", () => {
    expect(
      editCommand(
        "update",
        { resource: { kind: "volume", name: "site" }, spec: { kind: { case: "volume", value: {} } } },
        { environment: "staging", on: ["srv_1", "srv_2"] },
      ),
    ).toBe("dockwell volumes update site --environment staging --on srv_1 --on srv_2");
  });

  it("writes a list, an enum and a flag that's on as the CLI takes them", () => {
    const command = editCommand("create", {
      resource: { kind: "service", name: "web" },
      spec: {
        kind: {
          case: "service",
          value: {
            runs: {
              case: "app",
              value: {
                image: "web:1",
                hostNetwork: true,
                restart: RestartPolicy.ON_FAILURE,
                healthcheck: { test: ["CMD", "true"], interval: "30s" },
              },
            },
          },
        },
      },
    });
    expect(command).toContain(" --app-healthcheck-test CMD --app-healthcheck-test true");
    expect(command).toContain(" --app-healthcheck-interval 30s");
    expect(command).toContain(" --app-restart on-failure");
    expect(command).toContain(" --app-host-network");
  });
});

describe("methodCommand", () => {
  it("puts a request's id first, and each field as a flag", () => {
    expect(methodCommand(ServiceService.method.deployService, { id: "web", image: "nginx:1.28", summary: "" })).toBe(
      "dockwell services deploy web --image nginx:1.28",
    );
  });

  it("writes a message as JSON", () => {
    expect(
      methodCommand(WebsiteService.method.createWebsite, {
        domain: "blog.example.com",
        website: { serves: { case: "proxy", value: { port: 3000 } } },
      }),
    ).toBe(`dockwell websites create --domain blog.example.com --website '{"proxy":{"port":3000}}'`);
  });
});
