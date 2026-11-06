// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

import { refusedField } from "./domains";

/**
 * What the server says, from its source, with values in place of its verbs, so this
 * can't go on passing with words the server no longer uses.
 */
function saidIn(file: string, about: RegExp): string[] {
  const source = readFileSync(resolve(__dirname, "../../../..", file), "utf8");
  return [...source.matchAll(/fmt\.Errorf\("((?:[^"\\]|\\.)*)"/g)]
    .map(([, format]) => format ?? "")
    .filter((format) => about.test(format))
    .map((format) =>
      format
        .replace(/\\"/g, '"')
        .replace(/%\[?\d*\]?d/g, "50")
        .replace(/%\[?\d*\]?s/g, "a.example.net"),
    );
}

describe("which field a refusal lands on", () => {
  const form = { domain: "app.example.com", port: "8080", onServer: false };

  it("goes above the fields for a limit, as the server words each one", () => {
    const refusals = saidIn("internal/ingress/limits.go", /certificate/).filter((said) =>
      /\blimit\b|at most/.test(said),
    );
    expect(refusals.length).toBeGreaterThanOrEqual(2);
    expect(refusals.some((said) => said.includes("A domain stops counting"))).toBe(true);
    for (const said of refusals) expect(refusedField(said, form)).toBe("");
  });

  it("goes above the fields for the limit on what was asked", () => {
    const said =
      "150 domains were added to this server in the last 3 hours with no certificate come yet, some of them since removed or served without one, and this change adds 1 more, past the limit of 150. Each asked the certificate authority for a certificate, which removing its route doesn't take back, and Let's Encrypt allows 300 new orders for each account in 3 hours, the panel's domain's too. Point a domain's DNS here before you add it, or wait: the first of them stops counting in 2 h";
    expect(refusedField(said, form)).toBe("");
  });

  it("goes above the fields for the panel's domain moving", () => {
    const refusals = saidIn("internal/ingress/ingress.go", /^this plan didn't say/);
    expect(refusals).toHaveLength(1);
    expect(refusedField(refusals[0] ?? "", form)).toBe("");
  });

  it("lands on Domain for what's about what you typed", () => {
    expect(refusedField("app.example.com is the panel's own domain, so no route can claim it", form)).toBe("domain");
    expect(refusedField("a route needs a domain, such as app.example.com", { ...form, domain: "" })).toBe("domain");
    expect(refusedField("the domain aaaa… is longer than the 253 characters DNS allows", form)).toBe("domain");
  });

  it("names a proxy's field and a port's as before", () => {
    expect(refusedField("a route names 40 trusted proxies, and it takes at most 32", form)).toBe("proxies");
    expect(refusedField("the port its service listens on is out of range", form)).toBe("port");
  });
});
