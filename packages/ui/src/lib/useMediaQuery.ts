// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useSyncExternalStore } from "react";

/** The narrowest layout: phones, where the rail becomes a tab bar and overlays fill the screen. */
export const PHONE_QUERY = "(max-width: 639.98px)";

/**
 * Whether a media query matches, kept up to date. False where matchMedia
 * doesn't exist (a server render, jsdom), so the wide layout is the default.
 */
export function useMediaQuery(query: string): boolean {
  const subscribe = useCallback(
    (onChange: () => void) => {
      const media = globalThis.matchMedia?.(query);
      media?.addEventListener("change", onChange);
      return () => media?.removeEventListener("change", onChange);
    },
    [query],
  );
  return useSyncExternalStore(
    subscribe,
    () => globalThis.matchMedia?.(query).matches ?? false,
    () => false,
  );
}
