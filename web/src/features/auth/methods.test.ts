// SPDX-License-Identifier: AGPL-3.0-only

import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import type { DescMethod, DescService } from "@bufbuild/protobuf";
import * as api from "@dockwell/api";
import { Effect, Reach, Role } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { ruleOf } from "./may";
import { PANEL_METHODS } from "./methods";

/** Every service of the API, as the generated client has them. */
const services = (Object.values(api) as unknown[]).filter(
  (v): v is DescService => typeof v === "object" && v !== null && (v as { kind?: string }).kind === "service",
);

const methods = new Map<string, DescMethod>();
for (const service of services) {
  for (const method of service.methods) methods.set(`${service.name}.${method.localName}`, method);
}

/** The panel's own source, tests and their helpers left out. */
function source(dir: string): string {
  let out = "";
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name !== "test" && entry.name !== "locales") out += source(path);
    } else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name) && entry.name !== "methods.ts") {
      out += `${readFileSync(path, "utf8")}\n`;
    }
  }
  return out;
}

const panel = source(join(import.meta.dirname, "..", ".."));

/** The panel calls a method where its source names it: `.deployService(` on a client, or `Service.method.deployService`. */
const calls = (method: DescMethod) => new RegExp(`[.]${method.localName}\\b`).test(panel);

/** The panel asks `may` for a method where its source passes it to one: `may(ServiceService.method.deployService`. */
const asks = (method: DescMethod) =>
  new RegExp(`(?:\\bmay|useMay\\(\\))\\(\\s*${method.parent.name}[.]method[.]${method.localName}\\b`).test(panel);

const RANK = { [Role.UNSPECIFIED]: 0, [Role.VIEWER]: 1, [Role.DEVELOPER]: 2, [Role.ADMIN]: 3, [Role.OWNER]: 4 };

describe("what the panel does about each method", () => {
  it("finds the API's methods", () => {
    expect(methods.size).toBeGreaterThan(120);
  });

  it("has a decision for every method, and none for a method that's gone", () => {
    expect(Object.keys(PANEL_METHODS).sort()).toEqual([...methods.keys()].sort());
  });

  it.each([...methods.keys()])("%s is offered as its decision says", (name) => {
    const method = methods.get(name) as DescMethod;
    const use = PANEL_METHODS[name] ?? "unused";
    const rule = ruleOf(method);
    const own = rule.reach === Reach.ANYONE || rule.reach === Reach.SELF;
    const guarded = !own && (rule.effect !== Effect.READ || RANK[rule.needs] >= RANK[Role.ADMIN]);
    if (use === "unused") {
      expect(calls(method), "the panel calls it, so it needs a decision").toBe(false);
      return;
    }
    if (use === "open") {
      expect(own, "only a call before sign-in, or on your own account, is open").toBe(true);
      return;
    }
    if (use === "read") {
      expect(guarded, "a write, or a read that's an admin's, asks may").toBe(false);
      expect(own).toBe(false);
      return;
    }
    if (use === "plan") {
      // Only what acts on a plan already made, which carries the server's own answer.
      expect(["ChangeService.applyChange", "ChangeService.discardChange"]).toContain(name);
      return;
    }
    if (use === "may") {
      expect(asks(method), `nothing in the panel calls may(${method.parent.name}.method.${method.localName}`).toBe(
        true,
      );
      return;
    }
    // Within a screen: its way in asks may, and needs at least as much, on the install.
    const door = methods.get(use.within);
    expect(door, `${use.within} isn't a method`).toBeDefined();
    if (!door) return;
    expect(PANEL_METHODS[use.within]).toBe("may");
    const way = ruleOf(door);
    expect(way.reach).toBe(Reach.INSTALL);
    expect(RANK[way.needs]).toBeGreaterThanOrEqual(RANK[rule.needs]);
    expect(RANK[way.needs]).toBeGreaterThanOrEqual(RANK[rule.needsProtected]);
  });

  it("marks no method the panel calls as one it doesn't need to ask about", () => {
    for (const [name, method] of methods) {
      const use = PANEL_METHODS[name];
      if (use === "read" || use === "open")
        expect(calls(method), `${name} is marked ${use}, and the panel doesn't call it`).toBe(true);
    }
  });
});
