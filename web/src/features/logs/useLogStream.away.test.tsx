// SPDX-License-Identifier: AGPL-3.0-only
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import type { HandlerContext } from "@connectrpc/connect";
import { type WatchLogsResponse, WatchLogsResponseSchema } from "@dockwell/api";
import { act, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { HIDDEN_CLOSE_MS } from "../../lib/outOfSight";
import { renderWithApi, untilAborted } from "../../test/render";
import { LogStore } from "./store";
import { LONG_AWAY_MS, type StreamState, useLogStream } from "./useLogStream";

const seen = (state: "hidden" | "visible") => {
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
  document.dispatchEvent(new Event("visibilitychange"));
};
afterEach(() => {
  vi.useRealTimers();
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" });
});

let n = 0;
const line = (text: string, token: string) =>
  create(WatchLogsResponseSchema, {
    chunks: [{ sourceId: "c1", lines: [{ time: timestampFromDate(new Date(Date.now() + n++)), text }] }],
    resumeToken: token,
  });

function server(once = false) {
  const asked: string[] = [];
  let open = 0;
  let push: (m: WatchLogsResponse) => void = () => {};
  const watchLogs = async function* (req: { resumeToken: string }, ctx: HandlerContext) {
    asked.push(req.resumeToken);
    open++;
    try {
      const queue: WatchLogsResponse[] = [];
      let wake = () => {};
      push = (m) => {
        queue.push(m);
        wake();
      };
      yield create(WatchLogsResponseSchema, { caughtUp: true });
      if (once) return;
      for (;;) {
        const next = queue.shift();
        if (next) {
          yield next;
          continue;
        }
        if (ctx.signal.aborted) return;
        await Promise.race([new Promise<void>((r) => (wake = r)), untilAborted(ctx.signal)]);
      }
    } finally {
      open--;
    }
  };
  return { api: { logs: { watchLogs } }, asked, open: () => open, push: (m: WatchLogsResponse) => push(m) };
}

function Follower({ store, once }: { store: LogStore; once?: boolean }) {
  useLogStream(once ? { id: "svc_web", once: true } : { id: "svc_web" }, store);
  return null;
}

describe("a log out of sight", () => {
  it("keeps lines heard just before it closed, and asks again from the last token", async () => {
    const s = server();
    const store = new LogStore();
    const { unmount } = renderWithApi(<Follower store={store} />, s.api);
    await waitFor(() => expect(s.open()).toBe(1));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    await act(async () => s.push(line("one", "t1")));
    await act(() => vi.advanceTimersByTimeAsync(200));
    act(() => seen("hidden"));
    await act(() => vi.advanceTimersByTimeAsync(HIDDEN_CLOSE_MS - 50));
    // Heard within FLUSH_MS of the close, so still pending as it closes.
    await act(async () => s.push(line("two-a", "t2a")));
    await act(() => vi.advanceTimersByTimeAsync(10));
    await act(async () => s.push(line("two", "t2")));
    await act(() => vi.advanceTimersByTimeAsync(60));
    await waitFor(() => expect(s.open()).toBe(0));
    expect(store.received).toBe(3);
    act(() => seen("visible"));
    await waitFor(() => expect(s.open()).toBe(1));
    expect(s.asked).toEqual(["", "t2"]);
    await act(async () => s.push(line("three", "t3")));
    await act(() => vi.advanceTimersByTimeAsync(200));
    expect(store.received).toBe(4);
    unmount();
    await waitFor(() => expect(s.open()).toBe(0));
  });

  it("starts over with the tail when nothing had come before it closed", async () => {
    const s = server();
    const store = new LogStore();
    const { unmount } = renderWithApi(<Follower store={store} />, s.api);
    await waitFor(() => expect(s.open()).toBe(1));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    act(() => seen("hidden"));
    await act(() => vi.advanceTimersByTimeAsync(HIDDEN_CLOSE_MS + 100));
    await waitFor(() => expect(s.open()).toBe(0));
    act(() => seen("visible"));
    await waitFor(() => expect(s.open()).toBe(1));
    // No token: the server reads its tail again, not what came while away.
    expect(s.asked).toEqual(["", ""]);
    unmount();
  });

  it("hidden for less than the wait keeps the same stream", async () => {
    const s = server();
    const store = new LogStore();
    const { unmount } = renderWithApi(<Follower store={store} />, s.api);
    await waitFor(() => expect(s.open()).toBe(1));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    act(() => seen("hidden"));
    await act(() => vi.advanceTimersByTimeAsync(HIDDEN_CLOSE_MS - 1000));
    act(() => seen("visible"));
    act(() => seen("hidden"));
    await act(() => vi.advanceTimersByTimeAsync(HIDDEN_CLOSE_MS - 1000));
    expect(s.asked).toEqual([""]);
    expect(s.open()).toBe(1);
    unmount();
  });

  it("doesn't read a finished range again on coming back", async () => {
    const s = server(true);
    const store = new LogStore();
    const { unmount } = renderWithApi(<Follower store={store} once />, s.api);
    await waitFor(() => expect(s.asked).toHaveLength(1));
    await waitFor(() => expect(s.open()).toBe(0));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    act(() => seen("hidden"));
    await act(() => vi.advanceTimersByTimeAsync(HIDDEN_CLOSE_MS + 100));
    act(() => seen("visible"));
    await act(() => vi.advanceTimersByTimeAsync(500));
    expect(s.asked).toHaveLength(1);
    unmount();
  });

  it("says it's catching up after a while away, until the stream says it has", async () => {
    const s = server();
    const store = new LogStore();
    let state: StreamState | undefined;
    const catching: (boolean | undefined)[] = [];
    function Watching() {
      state = useLogStream({ id: "svc_web" }, store);
      catching.push(state.catchingUp);
      return null;
    }
    const { unmount } = renderWithApi(<Watching />, s.api);
    await waitFor(() => expect(s.open()).toBe(1));
    await act(async () => s.push(line("one", "t1")));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    act(() => seen("hidden"));
    await act(() => vi.advanceTimersByTimeAsync(HIDDEN_CLOSE_MS + 100));
    await waitFor(() => expect(s.open()).toBe(0));
    act(() => seen("visible"));
    // The server says it has caught up as it starts; until then, it says it's catching up.
    await waitFor(() => expect(s.asked).toEqual(["", "t1"]));
    await waitFor(() => expect(state?.catchingUp).toBe(false));
    expect(catching).toContain(true);
    expect(state?.missed).toBeUndefined();
    unmount();
    await waitFor(() => expect(s.open()).toBe(0));
  });

  it("starts again from the newest after long away, and marks what it didn't read", async () => {
    const s = server();
    const store = new LogStore();
    let state: StreamState | undefined;
    function Watching() {
      state = useLogStream({ id: "svc_web" }, store);
      return null;
    }
    const { unmount } = renderWithApi(<Watching />, s.api);
    await waitFor(() => expect(s.open()).toBe(1));
    await act(async () => s.push(line("one", "t1")));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    act(() => seen("hidden"));
    await act(() => vi.advanceTimersByTimeAsync(HIDDEN_CLOSE_MS + 100));
    await waitFor(() => expect(s.open()).toBe(0));
    await act(() => vi.advanceTimersByTimeAsync(LONG_AWAY_MS + 1000));
    act(() => seen("visible"));
    // From the newest, not from where it was: hours through the tunnel would come first.
    await waitFor(() => expect(s.asked).toEqual(["", ""]));
    expect(state?.missed?.to).toBeGreaterThan(state?.missed?.from ?? Number.POSITIVE_INFINITY);
    const entries = Array.from({ length: store.length }, (_, i) => store.entryAt(store.start + i));
    expect(entries.some((e) => e?.away !== undefined)).toBe(true);
    unmount();
    await waitFor(() => expect(s.open()).toBe(0));
  });
});
