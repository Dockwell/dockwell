// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  CloudProvider,
  NetworkShape,
  PublicAddressSource,
  ServerNetworkSchema as Schema,
  type ServerNetworkSchema,
} from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { aServer } from "../../test/servers";
import { domainAdviceOf, publicAddressOf } from "./network";

const network = (init: MessageInitShape<typeof ServerNetworkSchema>) => create(Schema, init);
const looked = {
  ipv4: { address: "203.0.113.7", source: PublicAddressSource.LOOKED_UP, unproven: true },
};
const asRecord = { address: "203.0.113.7", unproven: true };

describe("domainAdviceOf", () => {
  it("leads with the record on the internet", () => {
    expect(
      domainAdviceOf(
        network({
          ipv4: { address: "203.0.113.52", source: PublicAddressSource.ON_INTERFACE },
          shape: NetworkShape.INTERNET,
        }),
      ),
    ).toEqual({ lead: "record", records: [{ address: "203.0.113.52", unproven: false }], provider: undefined });
  });

  it("offers a way out on a cloud as a record that's unproven, and names the provider", () => {
    const provider = CloudProvider.ORACLE;
    expect(domainAdviceOf(network({ ...looked, shape: NetworkShape.CLOUD_NAT, provider }))).toEqual({
      lead: "record",
      records: [asRecord],
      provider,
    });
  });

  it("names each family by what's known of it", () => {
    const ipv6 = { address: "2001:db8::5", source: PublicAddressSource.ON_INTERFACE };
    expect(domainAdviceOf(network({ ...looked, ipv6, shape: NetworkShape.LOCAL_NAT }))).toEqual({
      lead: "record",
      records: [asRecord, { address: "2001:db8::5", unproven: false }],
      provider: undefined,
    });
  });

  it("leaves a way out off behind carrier NAT, beside a way in", () => {
    const ipv6 = { address: "2001:db8::5", source: PublicAddressSource.ON_INTERFACE };
    expect(domainAdviceOf(network({ ...looked, ipv6, shape: NetworkShape.CARRIER_NAT }))).toEqual({
      lead: "record",
      records: [{ address: "2001:db8::5", unproven: false }],
      provider: undefined,
    });
  });

  it("asks on a cloud with no address known, as Oracle's metadata has none", () => {
    const provider = CloudProvider.ORACLE;
    expect(domainAdviceOf(network({ shape: NetworkShape.CLOUD_NAT, provider }))).toEqual({ lead: "ask", provider });
  });

  it("leads with a tunnel on a home or office network, and offers the router's address second", () => {
    expect(domainAdviceOf(network({ ...looked, shape: NetworkShape.LOCAL_NAT }))).toEqual({
      lead: "tunnel",
      router: "203.0.113.7",
    });
    expect(domainAdviceOf(network({ shape: NetworkShape.LOCAL_NAT }))).toEqual({ lead: "tunnel", router: undefined });
  });

  it("offers only a tunnel behind carrier NAT", () => {
    expect(domainAdviceOf(network({ ...looked, shape: NetworkShape.CARRIER_NAT }))).toEqual({ lead: "tunnel-only" });
  });

  it("takes a way in over the shape, as someone proved or said so", () => {
    for (const source of [PublicAddressSource.SET_BY_PERSON, PublicAddressSource.PROVED_BY_DOMAIN]) {
      for (const shape of [NetworkShape.LOCAL_NAT, NetworkShape.CARRIER_NAT]) {
        expect(domainAdviceOf(network({ ipv4: { address: "198.51.100.7", source }, shape })).lead).toBe("record");
      }
    }
  });

  it("asks while nothing is known", () => {
    expect(domainAdviceOf(undefined)).toEqual({ lead: "ask", provider: undefined });
    expect(domainAdviceOf(network({ ...looked }))).toEqual({ lead: "ask", provider: undefined });
  });
});

describe("publicAddressOf", () => {
  const server = (init: MessageInitShape<typeof ServerNetworkSchema>) => aServer({ network: network(init) });

  it("is what's known of a way in", () => {
    const ipv4 = { address: "198.51.100.7", source: PublicAddressSource.SET_BY_PERSON };
    expect(publicAddressOf(server({ ipv4 }))).toBe("198.51.100.7");
  });

  it("is never a way out, as that may be a router's or a NAT gateway's", () => {
    expect(publicAddressOf(server({ ...looked, shape: NetworkShape.LOCAL_NAT }))).toBeUndefined();
    expect(publicAddressOf(server({ ...looked, shape: NetworkShape.CLOUD_NAT }))).toBeUndefined();
    expect(publicAddressOf(server({}))).toBeUndefined();
  });
});
