// SPDX-License-Identifier: AGPL-3.0-only

import { afterEach, describe, expect, it, vi } from "vitest";

import { keepTrying, OFFLINE } from "./retry";

afterEach(() => vi.useRealTimers());

describe("keepTrying", () => {
  it("waits longer after each failure, and starts over once connected", async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, "random").mockReturnValue(0);
    const abort = new AbortController();
    const tries: number[] = [];
    const started = Date.now();
    const done = keepTrying(abort.signal, { minMs: 100, maxMs: 400 }, async (connected) => {
      tries.push(Date.now() - started);
      // The fourth try hears from the server before it fails.
      if (tries.length === 4) connected();
      if (tries.length === 6) return "done";
      throw new Error("down");
    });
    await vi.runAllTimersAsync();
    await done;
    // 100, 200, then the most, 400; then from the start once connected.
    expect(tries).toEqual([0, 100, 300, 700, 800, 1000]);
  });

  it("ends a try as the browser goes offline, as a stream that stays open says nothing then", async () => {
    const abort = new AbortController();
    const heard: unknown[] = [];
    let tries = 0;
    const done = keepTrying(
      abort.signal,
      { minMs: 1, maxMs: 1 },
      async (_connected, attempt) => {
        tries++;
        if (tries > 1) return "done";
        await new Promise((_resolve, refuse) => attempt.addEventListener("abort", () => refuse(attempt.reason)));
        return undefined;
      },
      (err) => {
        heard.push(err);
        return undefined;
      },
    );
    await vi.waitFor(() => expect(tries).toBe(1));
    window.dispatchEvent(new Event("offline"));
    await done;
    expect(heard).toEqual([OFFLINE]);
    expect(tries).toBe(2);
  });

  it("stops on a failure that trying again won't fix, and when aborted", async () => {
    const abort = new AbortController();
    let tries = 0;
    await keepTrying(
      abort.signal,
      { minMs: 1, maxMs: 1 },
      async () => {
        tries++;
        throw new Error("gone");
      },
      () => "done",
    );
    expect(tries).toBe(1);
    abort.abort();
    await keepTrying(abort.signal, { minMs: 1, maxMs: 1 }, async () => {
      tries++;
      return undefined;
    });
    expect(tries).toBe(1);
  });
});
