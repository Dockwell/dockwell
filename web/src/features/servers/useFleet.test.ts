// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";
import { Connection, type WatchServersResponse } from "@dockwell/api";
import { act, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { feed, renderHookWithApi } from "../../test/render";
import { aServer, changed, gap, removed, sample, snapshotComplete } from "../../test/servers";
import { HISTORY_LENGTH } from "./history";
import { type FleetState, useFleet } from "./useFleet";

/** Renders useFleet against watches, one per connection, and records every state it publishes. */
function watchWith(...watches: ReturnType<typeof feed<WatchServersResponse>>[]) {
  const seen: FleetState[] = [];
  let calls = 0;
  const hook = renderHookWithApi(
    () => {
      const state = useFleet();
      seen.push(state);
      return state;
    },
    {
      servers: {
        watchServers: (req, ctx) => {
          const watch = watches[calls++];
          if (!watch) throw new Error("the test ran out of watches");
          return watch.stream(req, ctx);
        },
      },
    },
  );
  return { ...hook, seen, calls: () => calls };
}

const names = (state: FleetState) => (state.status === "connecting" ? [] : state.servers.map((s) => s.name));

afterEach(() => {
  vi.useRealTimers();
});

describe("useFleet", () => {
  it("publishes the snapshot whole, once it is complete", async () => {
    const watch = feed<WatchServersResponse>();
    const { result, seen } = watchWith(watch);
    expect(result.current).toEqual({ status: "connecting" });

    act(() =>
      watch.push(changed(aServer({ id: "srv_1", name: "hz-1" })), changed(aServer({ id: "srv_2", name: "hz-2" }))),
    );
    act(() => watch.push(snapshotComplete));

    await waitFor(() => expect(result.current.status).toBe("live"));
    expect(names(result.current)).toEqual(["hz-1", "hz-2"]);
    // Never a partial list on the way there.
    expect(seen.filter((s) => s.status === "live").map(names)).not.toContainEqual(["hz-1"]);
  });

  it("settles on an empty fleet", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(snapshotComplete);
    const { result } = watchWith(watch);
    await waitFor(() => expect(result.current).toEqual({ status: "live", servers: [] }));
  });

  it("appends each new sample to the server's history, keeping the latest", async () => {
    const history = Array.from({ length: HISTORY_LENGTH }, (_, i) => sample(i));
    const watch = feed<WatchServersResponse>();
    watch.push(changed(aServer({ id: "srv_1", name: "hz-1", stats: history.at(-1), history })), snapshotComplete);
    const { result } = watchWith(watch);
    await waitFor(() => expect(result.current.status).toBe("live"));

    act(() => watch.push(changed(aServer({ id: "srv_1", name: "hz-1", stats: sample(99) }))));

    await waitFor(() => {
      if (result.current.status !== "live") throw new Error("not live");
      const [server] = result.current.servers;
      expect(server?.stats?.cpuPercent).toBe(99);
      expect(server?.history).toHaveLength(HISTORY_LENGTH);
      expect(server?.history.at(0)?.cpuPercent).toBe(1);
      expect(server?.history.at(-1)?.cpuPercent).toBe(99);
    });
  });

  it("doesn't count a repeated sample twice", async () => {
    const latest = sample(40);
    const watch = feed<WatchServersResponse>();
    watch.push(
      changed(aServer({ id: "srv_1", name: "hz-1", stats: latest, history: [sample(30), latest] })),
      snapshotComplete,
    );
    const { result } = watchWith(watch);
    await waitFor(() => expect(result.current.status).toBe("live"));

    // The agent disconnects: the message carries the same latest sample.
    act(() =>
      watch.push(changed(aServer({ id: "srv_1", name: "hz-1", connection: Connection.UNREACHABLE, stats: latest }))),
    );

    await waitFor(() => {
      if (result.current.status !== "live") throw new Error("not live");
      const [server] = result.current.servers;
      expect(server?.connection).toBe(Connection.UNREACHABLE);
      expect(server?.history.map((s) => s.cpuPercent)).toEqual([30, 40]);
    });
  });

  it("starts over when a server reconnects with its history", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(aServer({ id: "srv_1", name: "hz-1", history: [sample(10), sample(20)] })), snapshotComplete);
    const { result } = watchWith(watch);
    await waitFor(() => expect(result.current.status).toBe("live"));

    act(() => watch.push(changed(aServer({ id: "srv_1", name: "hz-1", history: [sample(30)] }))));

    await waitFor(() => {
      if (result.current.status !== "live") throw new Error("not live");
      expect(result.current.servers[0]?.history.map((s) => s.cpuPercent)).toEqual([30]);
    });
  });

  it("starts the history over when a server reports again after an outage", async () => {
    const watch = feed<WatchServersResponse>();
    const before = [sample(10), sample(20)];
    watch.push(
      changed(aServer({ id: "srv_1", name: "hz-1", stats: before.at(-1), history: before })),
      snapshotComplete,
    );
    const { result } = watchWith(watch);
    await waitFor(() => expect(result.current.status).toBe("live"));

    gap(2 * 60 * 60_000);
    act(() => watch.push(changed(aServer({ id: "srv_1", name: "hz-1", stats: sample(30) }))));

    await waitFor(() => {
      if (result.current.status !== "live") throw new Error("not live");
      expect(result.current.servers[0]?.history.map((s) => s.cpuPercent)).toEqual([30]);
    });
  });

  it("drops a removed server", async () => {
    const hz2 = aServer({ id: "srv_2", name: "hz-2" });
    const watch = feed<WatchServersResponse>();
    watch.push(changed(aServer({ id: "srv_1", name: "hz-1" })), changed(hz2), snapshotComplete);
    const { result } = watchWith(watch);
    await waitFor(() => expect(names(result.current)).toEqual(["hz-1", "hz-2"]));

    act(() => watch.push(removed(hz2)));

    await waitFor(() => expect(names(result.current)).toEqual(["hz-1"]));
  });

  it("keeps what it knew while it reconnects, then swaps in the fresh snapshot", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const first = feed<WatchServersResponse>();
    const second = feed<WatchServersResponse>();
    first.push(changed(aServer({ id: "srv_1", name: "hz-1" })), snapshotComplete);
    const { result, calls } = watchWith(first, second);
    await waitFor(() => expect(result.current.status).toBe("live"));

    act(() => first.fail(new ConnectError("the watch fell behind", Code.Unavailable)));

    await waitFor(() => expect(result.current.status).toBe("reconnecting"));
    expect(names(result.current)).toEqual(["hz-1"]);
    if (result.current.status === "reconnecting") expect(result.current.error.code).toBe(Code.Unavailable);

    second.push(changed(aServer({ id: "srv_2", name: "hz-2" })), snapshotComplete);
    // The first retry waits between one and two seconds.
    await act(() => vi.advanceTimersByTimeAsync(2_000));

    await waitFor(() => expect(result.current.status).toBe("live"));
    expect(names(result.current)).toEqual(["hz-2"]);
    expect(calls()).toBe(2);
  });

  it("says why the first watch failed, and keeps trying", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const first = feed<WatchServersResponse>();
    const second = feed<WatchServersResponse>();
    first.fail(new ConnectError("not allowed", Code.PermissionDenied));
    second.push(snapshotComplete);
    const { result } = watchWith(first, second);

    await waitFor(() =>
      expect(result.current.status === "connecting" && result.current.error?.code).toBe(Code.PermissionDenied),
    );

    await act(() => vi.advanceTimersByTimeAsync(2_000));
    await waitFor(() => expect(result.current).toEqual({ status: "live", servers: [] }));
  });

  it("stops watching when it unmounts", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const watch = feed<WatchServersResponse>();
    watch.push(snapshotComplete);
    const { result, unmount, calls } = watchWith(watch);
    await waitFor(() => expect(result.current.status).toBe("live"));

    unmount();
    watch.fail(new ConnectError("gone", Code.Unavailable));
    await vi.advanceTimersByTimeAsync(30_000);

    expect(calls()).toBe(1);
  });
});
