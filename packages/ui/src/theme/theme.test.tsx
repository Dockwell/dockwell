// SPDX-License-Identifier: AGPL-3.0-only

import { act, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import {
  applyThemePreference,
  readThemePreference,
  setThemePreference,
  THEME_STORAGE_KEY,
  useResolvedTheme,
  useThemePreference,
} from "./theme";

function mockSystem(dark: boolean) {
  const listeners = new Set<() => void>();
  const media = {
    matches: dark,
    addEventListener: (_: string, fn: () => void) => listeners.add(fn),
    removeEventListener: (_: string, fn: () => void) => listeners.delete(fn),
  };
  // jsdom has no matchMedia, so stub it.
  vi.stubGlobal("matchMedia", () => media as unknown as MediaQueryList);
  return {
    set(nextDark: boolean) {
      media.matches = nextDark;
      for (const fn of listeners) fn();
    },
  };
}

describe("theme preference", () => {
  it("defaults to system, and ignores anything unexpected in storage", () => {
    expect(readThemePreference()).toBe("system");
    localStorage.setItem(THEME_STORAGE_KEY, "sepia");
    expect(readThemePreference()).toBe("system");
  });

  it("survives blocked storage", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("blocked", "SecurityError");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("blocked", "SecurityError");
    });
    expect(readThemePreference()).toBe("system");
    expect(() => setThemePreference("dark")).not.toThrow();
    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
  });

  it("sets data-theme for a choice and removes it for system", () => {
    applyThemePreference("light");
    expect(document.documentElement).toHaveAttribute("data-theme", "light");
    applyThemePreference("system");
    expect(document.documentElement).not.toHaveAttribute("data-theme");
  });

  it("stores the choice and updates hooks", () => {
    const { result } = renderHook(() => useThemePreference());
    expect(result.current[0]).toBe("system");
    act(() => result.current[1]("dark"));
    expect(result.current[0]).toBe("dark");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("dark");
    act(() => result.current[1]("system"));
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBeNull();
  });

  it("resolves system to what the operating system shows, live", () => {
    const system = mockSystem(false);
    const { result } = renderHook(() => useResolvedTheme());
    expect(result.current).toBe("light");
    act(() => system.set(true));
    expect(result.current).toBe("dark");
    act(() => setThemePreference("light"));
    expect(result.current).toBe("light");
  });

  it("follows a change made in another tab", () => {
    const { result } = renderHook(() => useThemePreference());
    act(() => {
      localStorage.setItem(THEME_STORAGE_KEY, "light");
      window.dispatchEvent(new StorageEvent("storage", { key: THEME_STORAGE_KEY }));
    });
    expect(result.current[0]).toBe("light");
    expect(document.documentElement).toHaveAttribute("data-theme", "light");
  });
});
