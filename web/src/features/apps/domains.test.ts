// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { CertificateState, type Domain } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { domainNeedsALook, groupOf, lookOf, refusedField } from "./domains";

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

  it("goes above the fields for a limit that names a longer name, so holds what you typed", () => {
    const refusals = saidIn("internal/ingress/limits.go", /waiting for a certificate/);
    expect(refusals.some((said) => said.includes("such as a.example.net"))).toBe(true);
    for (const said of refusals) expect(refusedField(said, { ...form, domain: "example.net" })).toBe("");
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

  it("lands on Domain for the whole name, whatever comes after it", () => {
    for (const said of [
      "APP.example.com is the panel's own domain",
      "a route already serves app.example.com/ in this environment",
      "this can't serve app.example.com: its certificate failed",
      "the route www serves app.example.com.",
    ]) {
      expect(refusedField(said, form)).toBe("domain");
    }
  });

  it("names a proxy's field and a port's as before", () => {
    expect(refusedField("a route names 40 trusted proxies, and it takes at most 32", form)).toBe("proxies");
    expect(refusedField("the port its service listens on is out of range", form)).toBe("port");
  });
});

describe("how a domain sorts in the list of them", () => {
  const onServer = (server: object = {}) =>
    ({
      name: "app.example.com",
      routes: [{ tls: "" }],
      servers: [{ serverId: "srv_1", ...server }],
    }) as unknown as Domain;
  const certificate = (state: CertificateState, message = "") => ({ status: { certificate: { state, message } } });
  const sorted = (domain: Domain) => groupOf(lookOf(domain));

  it("says serving only of a certificate a server has said it has", () => {
    expect(sorted(onServer(certificate(CertificateState.ISSUED)))).toBe("serving");
    expect(sorted(onServer())).toBe("waiting");
    expect(sorted(onServer(certificate(CertificateState.UNSPECIFIED)))).toBe("waiting");
    expect(sorted(onServer(certificate(CertificateState.PENDING)))).toBe("waiting");
  });

  it("sorts what's going wrong as failing, even while it serves", () => {
    expect(sorted(onServer(certificate(CertificateState.FAILED, "DNS has no record.")))).toBe("failing");
    expect(sorted(onServer(certificate(CertificateState.ISSUED, "Renewing it is failing.")))).toBe("failing");
    expect(sorted(onServer({ ingress: { message: "Something else holds port 80." } }))).toBe("failing");
    expect(sorted({ ...onServer(), servers: [] } as Domain)).toBe("failing");
  });

  it("says behind a tunnel only of what a server has said takes no certificate", () => {
    expect(sorted(onServer(certificate(CertificateState.NONE)))).toBe("tunnel");
  });

  it("says nothing of how it stands while a server isn't connected, whatever that server last said", () => {
    const issued = { ...certificate(CertificateState.ISSUED), stale: true };
    const failed = { ...certificate(CertificateState.FAILED, "DNS has no record."), stale: true };
    expect(sorted(onServer(issued))).toBe("not-connected");
    expect(sorted(onServer(failed))).toBe("not-connected");
  });

  it("asks for a look at what's failing and what nobody can say, not what's on its way", () => {
    const needs = (domain: Domain) => domainNeedsALook(lookOf(domain));
    expect(needs(onServer(certificate(CertificateState.FAILED, "DNS has no record.")))).toBe(true);
    expect(needs(onServer({ ...certificate(CertificateState.ISSUED), stale: true }))).toBe(true);
    expect(needs(onServer(certificate(CertificateState.ISSUED)))).toBe(false);
    expect(needs(onServer(certificate(CertificateState.PENDING)))).toBe(false);
    expect(needs(onServer(certificate(CertificateState.NONE)))).toBe(false);
  });
});
