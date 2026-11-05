// SPDX-License-Identifier: AGPL-3.0-only

import { createContext, type RefObject, useContext } from "react";

/**
 * Where a phone's search button is: in the header above the page, as M-Logs has it,
 * which the logs fill in with what opens their search.
 */
export const LogsSearchButton = createContext<RefObject<(() => void) | null> | null>(null);

export function useLogsSearchButton(): RefObject<(() => void) | null> | null {
  return useContext(LogsSearchButton);
}
