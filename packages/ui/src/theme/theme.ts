// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useSyncExternalStore } from "react";

/** What the person chose. "system" follows the operating system. */
export type ThemePreference = "system" | "light" | "dark";
export type ResolvedTheme = "light" | "dark";

/** Where the preference lives. web/public/theme.js reads the same key before first paint. */
export const THEME_STORAGE_KEY = "dockwell.theme";

const CHANGE_EVENT = "dockwell:themechange";
const DARK_QUERY = "(prefers-color-scheme: dark)";

function isPreference(value: unknown): value is ThemePreference {
  return value === "system" || value === "light" || value === "dark";
}

/** Reads the stored preference. Storage can be blocked or empty, so anything unexpected means "system". */
export function readThemePreference(): ThemePreference {
  try {
    const stored = globalThis.localStorage?.getItem(THEME_STORAGE_KEY);
    return isPreference(stored) ? stored : "system";
  } catch {
    return "system";
  }
}

/** Sets data-theme on root for an explicit choice, and removes it so CSS follows the system. */
export function applyThemePreference(preference: ThemePreference, root: HTMLElement = document.documentElement): void {
  if (preference === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", preference);
}

/** Stores, applies and announces a new preference. */
export function setThemePreference(preference: ThemePreference): void {
  try {
    if (preference === "system") globalThis.localStorage?.removeItem(THEME_STORAGE_KEY);
    else globalThis.localStorage?.setItem(THEME_STORAGE_KEY, preference);
  } catch {
    // Storage is unavailable (private mode, blocked site data); the choice lasts for this page only.
  }
  applyThemePreference(preference);
  window.dispatchEvent(new Event(CHANGE_EVENT));
}

/** Whether a storage event is about the theme; a null key means storage was cleared. */
function aboutTheme(e: StorageEvent): boolean {
  return e.key === THEME_STORAGE_KEY || e.key === null;
}

/**
 * Has this page take on a theme chosen in another tab, whatever screen it is
 * on. The panel calls it once as it starts; the hook alone listens only while
 * something that uses it is drawn. Returns what stops it.
 */
export function followThemeAcrossTabs(): () => void {
  const onStorage = (e: StorageEvent) => {
    if (aboutTheme(e)) applyThemePreference(readThemePreference());
  };
  window.addEventListener("storage", onStorage);
  return () => window.removeEventListener("storage", onStorage);
}

function subscribePreference(onChange: () => void): () => void {
  const onStorage = (e: StorageEvent) => {
    if (aboutTheme(e)) {
      applyThemePreference(readThemePreference());
      onChange();
    }
  };
  window.addEventListener(CHANGE_EVENT, onChange);
  window.addEventListener("storage", onStorage);
  return () => {
    window.removeEventListener(CHANGE_EVENT, onChange);
    window.removeEventListener("storage", onStorage);
  };
}

/** The current preference and a setter. Stays in sync across tabs. */
export function useThemePreference(): [ThemePreference, (preference: ThemePreference) => void] {
  const preference = useSyncExternalStore(subscribePreference, readThemePreference, () => "system" as const);
  return [preference, useCallback((p: ThemePreference) => setThemePreference(p), [])];
}

function subscribeSystem(onChange: () => void): () => void {
  const media = window.matchMedia?.(DARK_QUERY);
  media?.addEventListener("change", onChange);
  return () => media?.removeEventListener("change", onChange);
}

function systemTheme(): ResolvedTheme {
  return window.matchMedia?.(DARK_QUERY).matches ? "dark" : "light";
}

/** The theme actually showing: the preference, or the system's when the preference is "system". */
export function useResolvedTheme(): ResolvedTheme {
  const [preference] = useThemePreference();
  const system = useSyncExternalStore(subscribeSystem, systemTheme, () => "light" as const);
  return preference === "system" ? system : preference;
}
