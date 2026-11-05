// SPDX-License-Identifier: AGPL-3.0-only

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { renderHookWithApi } from "../test/render";
import { useTickingNow, useTimeWords } from "./time";

const words = () => renderHookWithApi(useTimeWords, {}).result.current;
const NOW = new Date(2026, 10, 4, 12, 0).getTime();
const before = (ms: number) => new Date(NOW - ms);

describe("useTimeWords", () => {
  it.each([
    [0, "1 s ago"],
    [400, "1 s ago"],
    [-2_000, "1 s ago"],
    [7_900, "7 s ago"],
    [3 * 60_000, "3 min ago"],
    [2 * 3_600_000, "2 h ago"],
    [3 * 86_400_000, "1 Nov"],
  ])("says %d ms ago as “%s”", (ms, text) => {
    expect(words().ago(before(ms), NOW)).toBe(text);
  });

  it.each([
    [40_000, "40 s"],
    [12 * 60_000, "12 min"],
    [5 * 3_600_000, "5 hours"],
    [41 * 86_400_000, "41 days"],
  ])("says a span of %d ms as “%s”", (ms, text) => {
    expect(words().span(ms)).toBe(text);
  });
});

describe("the time a screen shows ages by", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  const tick = (ms: number) =>
    act(() => {
      vi.advanceTimersByTime(ms);
    });

  it("moves each second while what it shows is under a minute old", () => {
    const start = Date.now();
    const { result } = renderHook(() => useTickingNow(start));
    tick(1_000);
    expect(result.current).toBe(start + 1_000);
    tick(1_000);
    expect(result.current).toBe(start + 2_000);
  });

  it("moves every 30 s after that, rendering no more than it has to", () => {
    const start = Date.now();
    let renders = 0;
    const { result } = renderHook(() => {
      renders++;
      return useTickingNow(start - 5 * 60_000);
    });
    const before = renders;
    tick(29_000);
    expect(result.current).toBe(start);
    expect(renders).toBe(before);
    tick(1_000);
    expect(result.current).toBe(start + 30_000);
    expect(renders).toBe(before + 1);
  });

  it("goes back to seconds when something newer shows", () => {
    const start = Date.now();
    const { result, rerender } = renderHook(({ newest }) => useTickingNow(newest), {
      initialProps: { newest: start - 5 * 60_000 },
    });
    tick(5_000);
    expect(result.current).toBe(start);
    rerender({ newest: start + 4_000 });
    tick(1_000);
    expect(result.current).toBe(start + 6_000);
  });

  it("shares one timer, however many screens listen, and stops with the last", () => {
    const set = vi.spyOn(globalThis, "setInterval");
    const clear = vi.spyOn(globalThis, "clearInterval");
    const one = renderHook(() => useTickingNow(undefined));
    const two = renderHook(() => useTickingNow(undefined));
    expect(set).toHaveBeenCalledTimes(1);
    one.unmount();
    expect(clear).not.toHaveBeenCalled();
    two.unmount();
    expect(clear).toHaveBeenCalledTimes(1);
    set.mockRestore();
    clear.mockRestore();
  });
});
