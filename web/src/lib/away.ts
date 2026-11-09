// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState } from "react";

/**
 * How long a page may be out of sight before what it watches only so someone sees
 * it stops: a tab left in the background, or a phone's locked browser.
 */
export const AWAY_AFTER_MS = 60_000;

/**
 * Whether the page has been out of sight for at least ms, as a tab in the
 * background or a locked phone's browser is. It's false again as soon as the page
 * is seen.
 */
export function useAwayFor(ms: number = AWAY_AFTER_MS): boolean {
  const [away, setAway] = useState(false);
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const look = () => {
      clearTimeout(timer);
      if (document.visibilityState === "hidden") timer = setTimeout(() => setAway(true), ms);
      else setAway(false);
    };
    look();
    document.addEventListener("visibilitychange", look);
    return () => {
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", look);
    };
  }, [ms]);
  return away;
}
