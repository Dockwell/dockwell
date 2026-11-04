// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { ConnectError } from "@connectrpc/connect";
import { Connection, ServerInventorySchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { aServer, sample } from "../../test/servers";
import { fleetView, serverStatus } from "./server";

const hz1 = aServer({
  id: "srv_1",
  name: "hz-1",
  stats: sample(10),
  inventory: create(ServerInventorySchema, { addresses: ["10.0.0.2", "49.12.33.7"] }),
});
const hz2 = aServer({ id: "srv_2", name: "hz-2", stats: sample(20) });

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
    expect(after.counts).toEqual({ connected: 1, down: 1, waiting: 0, unknown: 0 });
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
    expect(view.counts).toEqual({ connected: 1, down: 1, waiting: 1, unknown: 2 });
  });
});

describe("serverStatus", () => {
  it("draws a state it doesn't know as stopped", () => {
    expect(serverStatus({ connection: Connection.UNSPECIFIED })).toBe("stopped");
    expect(serverStatus({ connection: 9 as Connection })).toBe("stopped");
  });
});
