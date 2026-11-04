// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState } from "react";

/**
 * How long work may run before the panel says it is waiting, so fast answers
 * never flash a loading message.
 */
export const LOADING_DELAY_MS = 300;

/** True once `active` has stayed true for `ms`; false again as soon as it turns false. */
export function useElapsed(active: boolean, ms: number = LOADING_DELAY_MS): boolean {
  const [elapsed, setElapsed] = useState(false);
  useEffect(() => {
    if (!active) {
      setElapsed(false);
      return;
    }
    const timer = setTimeout(() => setElapsed(true), ms);
    return () => clearTimeout(timer);
  }, [active, ms]);
  return elapsed;
}
