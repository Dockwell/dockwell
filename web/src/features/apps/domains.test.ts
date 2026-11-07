// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { CertificateState, type Domain } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { groupConcern, groupNeedsALook, groupOf, lookOf, refusedField } from "./domains";

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
  const onServer = (server: object = {}, route: object = {}) =>
    ({
      name: "app.example.com",
      routes: [{ tls: "", ...route }],
      servers: [{ serverId: "srv_1", serverName: "hz-1", ...server }],
    }) as unknown as Domain;
  const certificate = (state: CertificateState, message = "", expireTime?: { seconds: bigint }) => ({
    status: { certificate: { state, message, expireTime } },
  });
  const sorted = (domain: Domain, silent = false) => groupOf(domain, lookOf(domain), silent);
  const needs = (domain: Domain, silent = false) => groupNeedsALook(sorted(domain, silent));
  const inDays = (days: number) => ({ seconds: BigInt(Math.floor(Date.now() / 1000) + days * 86_400) });

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

  it("says behind a tunnel only of what something ends TLS in front of", () => {
    expect(sorted(onServer(certificate(CertificateState.NONE), { tls: "off" }))).toBe("tunnel");
  });

  it("sorts what answers anyone over plain HTTP as serving, as nothing is in front of it", () => {
    const plain = onServer(certificate(CertificateState.NONE), { tls: "off", plainForAnyone: true });
    expect(sorted(plain)).toBe("serving");
    expect(needs(plain)).toBe(false);
  });

  it("says nothing of how it stands while a server isn't connected, whatever that server last said", () => {
    const issued = { ...certificate(CertificateState.ISSUED), stale: true };
    const failed = { ...certificate(CertificateState.FAILED, "DNS has no record."), stale: true };
    expect(sorted(onServer(issued))).toBe("not-connected");
    expect(sorted(onServer(failed))).toBe("not-connected");
  });

  it("sorts a server that has said nothing for a while apart, not as on its way", () => {
    expect(sorted(onServer(), false)).toBe("waiting");
    expect(sorted(onServer(), true)).toBe("no-report");
    expect(needs(onServer(), false)).toBe(false);
    expect(needs(onServer(), true)).toBe(true);
  });

  it("asks for a look at what's failing and what nobody can say, not what's on its way", () => {
    expect(needs(onServer(certificate(CertificateState.FAILED, "DNS has no record.")))).toBe(true);
    expect(needs(onServer({ ...certificate(CertificateState.ISSUED), stale: true }))).toBe(true);
    expect(needs(onServer(certificate(CertificateState.ISSUED)))).toBe(false);
    expect(needs(onServer(certificate(CertificateState.PENDING)))).toBe(false);
    expect(needs(onServer(certificate(CertificateState.NONE), { tls: "off" }))).toBe(false);
  });

  it("puts what needs a look first, then what's moving, then the rest", () => {
    expect(groupConcern("failing")).toBeLessThan(groupConcern("waiting"));
    expect(groupConcern("no-report")).toBe(groupConcern("failing"));
    expect(groupConcern("not-connected")).toBe(groupConcern("failing"));
    expect(groupConcern("waiting")).toBeLessThan(groupConcern("serving"));
    expect(groupConcern("serving")).toBe(groupConcern("tunnel"));
  });

  it("reads a certificate past its end as expired, whatever its server last said", () => {
    const out = onServer(certificate(CertificateState.ISSUED, "", inDays(-2)));
    const look = lookOf(out);
    expect(look.state).toBe("failed");
    expect(look.expired).toMatchObject({ id: "srv_1", name: "hz-1" });
    expect(sorted(out)).toBe("failing");
  });

  it("reads a certificate as good right up to its end", () => {
    const end = inDays(1);
    const domain = onServer(certificate(CertificateState.ISSUED, "", end));
    const at = Number(end.seconds) * 1000;
    expect(lookOf(domain, at - 1000).state).toBe("serving");
    expect(lookOf(domain, at).expired).toBeDefined();
  });

  it("goes by the control plane's clock where it's given, not this browser's", () => {
    const domain = onServer(certificate(CertificateState.ISSUED, "", inDays(30)));
    expect(lookOf(domain, Date.now() + 40 * 86_400_000).expired).toBeDefined();
    expect(lookOf(domain).expired).toBeUndefined();
  });

  it("goes by the soonest of a domain's servers, as it needs them all", () => {
    const issued = (id: string, days: number) => ({
      serverId: id,
      serverName: id,
      ...certificate(CertificateState.ISSUED, "", inDays(days)),
    });
    const domain = { ...onServer(), servers: [issued("late", 30), issued("gone", -1)] } as unknown as Domain;
    expect(lookOf(domain).expired).toMatchObject({ id: "gone" });
  });

  it("doesn't call a certificate expired behind one still on its way elsewhere", () => {
    const domain = {
      ...onServer(),
      servers: [
        { serverId: "a", serverName: "a", ...certificate(CertificateState.ISSUED, "", inDays(-1)) },
        { serverId: "b", serverName: "b", ...certificate(CertificateState.PENDING) },
      ],
    } as unknown as Domain;
    expect(lookOf(domain).expired).toMatchObject({ id: "a" });
  });

  it("leaves a certificate that says no end alone", () => {
    expect(lookOf(onServer(certificate(CertificateState.ISSUED))).state).toBe("serving");
  });
});
