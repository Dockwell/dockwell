// SPDX-License-Identifier: AGPL-3.0-only

import { Connection } from "@dockwell/api";
import { serverStatus } from "./server";
import type { useFleetView } from "./useFleet";

/** A server as the shell shows it. */
type FleetServer = NonNullable<ReturnType<typeof useFleetView>["servers"]>[number];

/**
 * The server something new runs on unless a person says otherwise, as there is no
 * scheduler yet (PRD principle 4): of those connected that can take it, healthy
 * first, the one with the most free room by `free`, such as memory for an app
 * (#792) and disk for a website (#798), and why. Undefined where none can.
 */
export function defaultServer(
  servers: readonly FleetServer[],
  free: (id: string) => number,
  unfit?: (server: FleetServer) => string | undefined,
): { id: string; why: "only" | "memory" } | undefined {
  const can = servers.filter((s) => s.connection === Connection.CONNECTED && unfit?.(s) === undefined);
  // A degraded server comes after every healthy one (the review's L3).
  const worse = (s: FleetServer) => (serverStatus(s) === "degraded" ? 1 : 0);
  const best = [...can].sort(
    (a, b) => worse(a) - worse(b) || free(b.id) - free(a.id) || a.name.localeCompare(b.name),
  )[0];
  if (!best) return undefined;
  return { id: best.id, why: can.length === 1 ? "only" : "memory" };
}
