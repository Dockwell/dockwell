// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { ConnectError } from "@connectrpc/connect";
import { Connection, ServerInventorySchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import addresses from "../../../../internal/resource/testdata/addresses.json";
import { aServer, sample } from "../../test/servers";
import { fleetView, isPrivateAddress, primaryAddress, serverStatus } from "./server";

const hz1 = aServer({
  id: "srv_1",
  name: "hz-1",
  stats: sample(10),
  inventory: create(ServerInventorySchema, { addresses: ["10.0.0.2", "49.12.33.7"] }),
});
const hz2 = aServer({ id: "srv_2", name: "hz-2", stats: sample(20) });

describe("isPrivateAddress", () => {
  // The table Go's resource.IsPublicAddress reads, so the two can't drift apart.
  it.each(addresses)("$address", ({ address, public: isPublic }) => {
    expect(isPrivateAddress(address)).toBe(!isPublic);
  });

  it("takes a server's name for no address", () => {
    expect(isPrivateAddress("10.hz-1")).toBe(false);
    expect(isPrivateAddress("hz-1")).toBe(false);
  });

  it("reads an address however it's written", () => {
    expect(isPrivateAddress("0:0:0:0:0:0:0:1")).toBe(true);
    expect(isPrivateAddress("2001:0DB8::1")).toBe(false);
    expect(isPrivateAddress("64:FF9B:1::1")).toBe(true);
  });

  it("doesn't take a TUN's address, as mihomo's, for where to reach a server", () => {
    expect(primaryAddress(["172.16.0.5", "198.18.0.1"])).toBe("172.16.0.5");
  });
});

describe("fleetView", () => {
  it("knows nothing until the first snapshot", () => {
    const view = fleetView({ status: "connecting" });
    expect(view.servers).toBeUndefined();
    expect(view.fleet).toBe(false);
    expect(view.only).toBeUndefined();
  });

  it("names the one server and where to reach it", () => {
    const view = fleetView({ status: "live", servers: [hz1] });
    expect(view.only).toEqual({ id: "srv_1", name: "hz-1", connection: Connection.CONNECTED, address: "49.12.33.7" });
    expect(view.fleet).toBe(false);
  });

  it("stays the same object while only the numbers change", () => {
    const before = fleetView({ status: "live", servers: [hz1, hz2] });
    const after = fleetView(
      { status: "live", servers: [aServer({ ...hz1, stats: sample(90) }), aServer({ ...hz2, stats: sample(5) })] },
      before,
    );
    expect(after).toBe(before);
  });

  it("changes when a server drops, keeping the parts that didn't", () => {
    const before = fleetView({ status: "live", servers: [hz1, hz2] });
    const after = fleetView(
      { status: "live", servers: [hz1, aServer({ ...hz2, connection: Connection.UNREACHABLE })] },
      before,
    );
    expect(after).not.toBe(before);
    expect(after.servers?.[0]).toBe(before.servers?.[0]);
    expect(after.counts).toEqual({ connected: 1, down: 1, waiting: 0, unknown: 0, docker: 0 });
  });

  it("changes when the watch starts reconnecting", () => {
    const before = fleetView({ status: "live", servers: [hz1, hz2] });
    const after = fleetView({ status: "reconnecting", servers: [hz1, hz2], error: new ConnectError("gone") }, before);
    expect(after.status).toBe("reconnecting");
    expect(after.servers).toBe(before.servers);
  });

  it("counts a state it doesn't know apart from the others", () => {
    const view = fleetView({
      status: "live",
      servers: [
        hz1,
        aServer({ id: "a", connection: Connection.PENDING }),
        aServer({ id: "b", connection: Connection.UNREACHABLE }),
        aServer({ id: "c", connection: Connection.UNSPECIFIED }),
        aServer({ id: "d", connection: 9 as Connection }),
      ],
    });
    expect(view.counts).toEqual({ connected: 1, down: 1, waiting: 1, unknown: 2, docker: 0 });
  });

  it("counts a connected server whose agent can't read Docker, and changes when it can again", () => {
    const since = new Date(Date.now() - 60_000);
    const blind = aServer({ ...hz2, dockerUnreadableSince: timestampFromDate(since) });
    const before = fleetView({ status: "live", servers: [hz1, blind] });
    expect(before.counts).toEqual({ connected: 2, down: 0, waiting: 0, unknown: 0, docker: 1 });
    expect(before.servers?.[1]?.dockerUnreadableSince).toBe(since.getTime());
    const after = fleetView({ status: "live", servers: [hz1, hz2] }, before);
    expect(after).not.toBe(before);
    expect(after.counts.docker).toBe(0);
    expect(after.servers?.[1]?.dockerUnreadableSince).toBeUndefined();
  });
});

describe("serverStatus", () => {
  it("draws a state it doesn't know as stopped", () => {
    expect(serverStatus({ connection: Connection.UNSPECIFIED })).toBe("stopped");
    expect(serverStatus({ connection: 9 as Connection })).toBe("stopped");
  });

  it("never draws a server whose agent can't read Docker as healthy", () => {
    expect(serverStatus({ connection: Connection.CONNECTED, dockerUnreadableSince: 1 })).toBe("degraded");
    expect(serverStatus({ connection: Connection.CONNECTED })).toBe("healthy");
    // Gone is gone, whatever it last said of Docker.
    expect(serverStatus({ connection: Connection.UNREACHABLE, dockerUnreadableSince: 1 })).toBe("down");
  });
});
