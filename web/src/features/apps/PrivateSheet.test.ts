// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { DomainRouteSchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { listBlocked, looksLikeEntry, makePassword, ownEntries } from "./PrivateSheet";

describe("who gets in to a route", () => {
  it("offers your IPv4 address alone, your IPv6 network first, and nothing from loopback", () => {
    expect(ownEntries("198.51.100.23")).toEqual(["198.51.100.23"]);
    expect(ownEntries("::ffff:198.51.100.23")).toEqual(["198.51.100.23"]);
    expect(ownEntries("2001:db8:0:12:a1b2:c3d4:e5f6:7")).toEqual([
      "2001:db8:0:12::/64",
      "2001:db8:0:12:a1b2:c3d4:e5f6:7",
    ]);
    // As the server writes them back, so the list doesn't change on its way there.
    expect(ownEntries("2001:DB8::7")).toEqual(["2001:db8::/64", "2001:db8::7"]);
    expect(ownEntries("2001:db8:0:0:1:0:0:7")).toEqual(["2001:db8::/64", "2001:db8::1:0:0:7"]);
    expect(ownEntries("fe80::1%eth0")[0]).toBe("fe80::/64");
    for (const loopback of ["127.0.0.1", "::1", "", "localhost"]) expect(ownEntries(loopback)).toEqual([]);
  });

  it("takes what has the shape of an address or a range, and leaves the rules to the server", () => {
    for (const entry of ["203.0.113.7", "203.0.113.0/24", "2001:db8::/64", "::1", "2001:db8:0:0:0:0:0:1/128"]) {
      expect(looksLikeEntry(entry)).toBe(true);
    }
    for (const entry of [
      "",
      "example.com",
      "203.0.113.0/24/1",
      "203.0.113.0/x",
      "{http.request.remote.host}",
      "2001:db8::1::2",
    ]) {
      expect(looksLikeEntry(entry)).toBe(false);
    }
  });

  it("makes a password of 24 letters and digits nobody mixes up, never twice", () => {
    const one = makePassword();
    expect(one).toMatch(/^[A-HJ-NP-Za-km-z2-9]{24}$/);
    expect(makePassword()).not.toBe(one);
  });

  it("can't keep strangers out by address behind Cloudflare's word, or a tunnel the route doesn't name", () => {
    expect(listBlocked(create(DomainRouteSchema, { trustedProxies: ["cloudflare"], tls: "off" }))).toBe("cloudflare");
    expect(listBlocked(create(DomainRouteSchema, { tls: "off" }))).toBe("tunnel");
    expect(listBlocked(create(DomainRouteSchema, { tls: "off", plainForAnyone: true }))).toBeUndefined();
    expect(listBlocked(create(DomainRouteSchema, { tls: "off", trustedProxies: ["127.0.0.1/32"] }))).toBeUndefined();
    expect(listBlocked(create(DomainRouteSchema, {}))).toBeUndefined();
  });
});
