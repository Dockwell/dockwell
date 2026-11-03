// SPDX-License-Identifier: AGPL-3.0-only

import { createContext, type ReactNode, useContext } from "react";

import type { Status } from "./status";

/**
 * The words components say on their own: status names and accessible labels.
 * English by default; an app passes translations through UIStringsProvider,
 * so this package needs no i18n library.
 */
export interface UIStrings {
  status: Readonly<Record<Status, string>>;
  /** Accessible name of a tag's remove button, given the tag's text. */
  remove: (what: string) => string;
}

export const englishStrings: UIStrings = {
  status: {
    healthy: "Healthy",
    degraded: "Degraded",
    down: "Down",
    deploying: "Deploying",
    stopped: "Stopped",
  },
  remove: (what) => `Remove ${what}`,
};

const StringsContext = createContext<UIStrings>(englishStrings);

export function UIStringsProvider({ strings, children }: { strings: UIStrings; children: ReactNode }) {
  return <StringsContext value={strings}>{children}</StringsContext>;
}

export function useUIStrings(): UIStrings {
  return useContext(StringsContext);
}
