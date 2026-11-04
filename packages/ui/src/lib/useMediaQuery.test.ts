// SPDX-License-Identifier: AGPL-3.0-only

import { act, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { useMediaQuery } from "./useMediaQuery";

/** A matchMedia that counts the lists it makes and lets the test flip whether they match. */
function stubMatchMedia() {
  let matches = false;
  const listeners = new Set<() => void>();
  const matchMedia = vi.fn((media: string) => ({
    media,
    get matches() {
      return matches;
    },
    addEventListener: (_type: "change", listener: () => void) => listeners.add(listener),
    removeEventListener: (_type: "change", listener: () => void) => listeners.delete(listener),
  }));
  vi.stubGlobal("matchMedia", matchMedia);
  return {
    matchMedia,
    listeners,
    set(next: boolean) {
      matches = next;
      for (const listener of listeners) listener();
    },
  };
}

describe("useMediaQuery", () => {
  it("parses the query once, however often it renders", () => {
    const media = stubMatchMedia();
    const { result, rerender } = renderHook(() => useMediaQuery("(max-width: 639.98px)"));
    for (let i = 0; i < 5; i++) rerender();
    expect(result.current).toBe(false);
    expect(media.matchMedia).toHaveBeenCalledTimes(1);
  });

  it("follows the query as it starts and stops matching", () => {
    const media = stubMatchMedia();
    const { result, unmount } = renderHook(() => useMediaQuery("(max-width: 639.98px)"));
    act(() => media.set(true));
    expect(result.current).toBe(true);
    act(() => media.set(false));
    expect(result.current).toBe(false);
    unmount();
    expect(media.listeners.size).toBe(0);
  });

  it("parses a new query when it changes", () => {
    const media = stubMatchMedia();
    const { rerender } = renderHook(({ query }) => useMediaQuery(query), { initialProps: { query: "(a)" } });
    rerender({ query: "(b)" });
    expect(media.matchMedia.mock.calls.map(([query]) => query)).toEqual(["(a)", "(b)"]);
  });

  it("says false where there is no matchMedia", () => {
    vi.stubGlobal("matchMedia", undefined);
    const { result } = renderHook(() => useMediaQuery("(max-width: 639.98px)"));
    expect(result.current).toBe(false);
  });
});
