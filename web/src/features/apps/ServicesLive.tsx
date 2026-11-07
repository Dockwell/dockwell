// SPDX-License-Identifier: AGPL-3.0-only

import { createContext, type ReactNode, useContext } from "react";

import { useServicesLive } from "./useServices";

const Live = createContext<{ reconnecting: boolean }>({ reconnecting: false });

/**
 * Keeps the services up to date on every screen of the shell, with one watch:
 * an app made from the CLI shows on the overview, in the rail and on Apps
 * alike, as soon as its change applies.
 */
export function ServicesLive({ children }: { children: ReactNode }) {
  return <Live.Provider value={useServicesLive()}>{children}</Live.Provider>;
}

/** Whether the shell's watch of the services is reconnecting, so what shows may be a little old. */
export function useServicesWatch(): { reconnecting: boolean } {
  return useContext(Live);
}
