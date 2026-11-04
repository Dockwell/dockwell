// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useMemo, useSyncExternalStore } from "react";

/** The narrowest layout: phones, where the rail becomes a tab bar and overlays fill the screen. */
export const PHONE_QUERY = "(max-width: 639.98px)";

/**
 * Whether a media query matches, kept up to date. False where matchMedia
 * doesn't exist (a server render, jsdom), so the wide layout is the default.
 * The query is parsed once; each render only reads whether it matches.
 */
export function useMediaQuery(query: string): boolean {
  const media = useMemo(() => globalThis.matchMedia?.(query), [query]);
  const subscribe = useCallback(
    (onChange: () => void) => {
      media?.addEventListener("change", onChange);
      return () => media?.removeEventListener("change", onChange);
    },
    [media],
  );
  return useSyncExternalStore(
    subscribe,
    () => media?.matches ?? false,
    () => false,
  );
}
