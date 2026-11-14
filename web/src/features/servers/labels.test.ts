// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { aServer } from "../../test/servers";
import {
  defaultGroupKey,
  fleetLabels,
  groupBy,
  keyProblem,
  labelChanges,
  labelSuggestions,
  matches,
  parseLabel,
  parseSelector,
  parseTerm,
  selectorText,
  valueProblem,
} from "./labels";

describe("labels", () => {
  // The cases internal/resource/labels_test.go checks, so the panel says what the server would.
  it("takes a key as a Kubernetes label's", () => {
    for (const key of [
      "region",
      "pool",
      "example.com/tier",
      "a",
      "A_b-c.d",
      "k".repeat(63),
      `${"p".repeat(60)}.com/x`,
    ]) {
      expect(keyProblem(key), key).toBeUndefined();
    }
    const cases: [string, string][] = [
      ["", "no-key"],
      ["/region", "empty-prefix"],
      ["Example.com/tier", "prefix-not-domain"],
      ["example.com/", "no-name"],
      ["-region", "ends"],
      ["region-", "ends"],
      ["re gion", "character"],
      ["re:gion", "character"],
      ["k".repeat(64), "long"],
      ["a/b/c", "character"],
      [`${"p".repeat(254)}/x`, "long-prefix"],
    ];
    for (const [key, kind] of cases) {
      expect(keyProblem(key)?.kind, key).toBe(kind);
    }
    for (const value of ["", "eu", "web-1", "1.2.3"]) expect(valueProblem(value), value).toBeUndefined();
    expect(valueProblem("eu/west")?.kind).toBe("character");
  });

  it("reads a label as key:value, or a key alone", () => {
    expect(parseLabel("region:eu")).toEqual({ key: "region", value: "eu" });
    expect(parseLabel(" gpu ")).toEqual({ key: "gpu", value: "" });
    expect(parseLabel("region:eu:west")).toEqual({ kind: "character", character: ":" });
    expect(parseLabel(":eu")).toEqual({ kind: "no-key" });
  });

  it("reads a selector, and matches servers by it", () => {
    const sel = parseSelector("pool:web  region !draining bad!term");
    expect(selectorText(sel)).toBe("pool:web region !draining");
    expect(parseTerm("!region:eu")).toEqual({ key: "region", value: "eu", hasValue: true, not: true });
    expect(matches(sel, { pool: "web", region: "eu" })).toBe(true);
    expect(matches(sel, { pool: "web", region: "eu", draining: "" })).toBe(false);
    expect(matches(sel, { pool: "api", region: "eu" })).toBe(false);
    expect(matches(sel, { pool: "web" })).toBe(false);
    expect(matches([], {})).toBe(true);
    expect(matches(parseSelector("region:"), { region: "" })).toBe(true);
  });

  it("offers what the fleet uses, groups by region first, and says what a change sets", () => {
    const fleet = [
      aServer({ id: "a", labels: { region: "eu", pool: "web" } }),
      aServer({ id: "b", labels: { region: "us", pool: "web" } }),
      aServer({ id: "c", labels: { pool: "db", gpu: "" } }),
      aServer({ id: "d" }),
    ];
    const used = fleetLabels(fleet);
    expect([...used.keys()]).toEqual(["pool", "region", "gpu"]);
    expect(used.get("pool")).toEqual(["db", "web"]);
    expect(defaultGroupKey(used)).toBe("region");
    expect(defaultGroupKey(fleetLabels([fleet[2] ?? aServer({})]))).toBe("gpu");
    expect(labelSuggestions(used, { pool: "web" })).toEqual([
      "pool:db",
      "region:eu",
      "region:us",
      "gpu",
      "pool:",
      "region:",
      "gpu:",
    ]);
    expect(groupBy(fleet, "region").map((g) => [g.value, g.servers.map((s) => s.id)])).toEqual([
      ["eu", ["a"]],
      ["us", ["b"]],
      [undefined, ["c", "d"]],
    ]);
    expect(labelChanges({ region: "eu", pool: "web" }, { region: "us", tier: "db" })).toEqual({
      set: ["region:us", "tier:db"],
      remove: ["pool"],
    });
  });
});
