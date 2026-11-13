// SPDX-License-Identifier: AGPL-3.0-only

import { createContext, useContext } from "react";

/**
 * Deploys the app whose page this is, as its header's Deploy does, for a tab whose
 * empty state asks for it (an app with no containers yet). Undefined off an app's
 * page, and where the reader may not deploy it, so a tab offers nothing it can't do.
 */
export const AppDeployAction = createContext<(() => void) | undefined>(undefined);

/** The app page's Deploy, or undefined where there's none to offer. */
export function useAppDeploy(): (() => void) | undefined {
  return useContext(AppDeployAction);
}
