// SPDX-License-Identifier: AGPL-3.0-only

import { Connection, type Server } from "@dockwell/api";
import type { Status } from "@dockwell/ui";

import type { FleetState } from "./useFleet";

const STATUS: Record<Connection, Status> = {
  [Connection.UNSPECIFIED]: "stopped",
  [Connection.CONNECTED]: "healthy",
  [Connection.UNREACHABLE]: "down",
  [Connection.PENDING]: "stopped",
};

/** A server's state as the design system draws it: connected is healthy, gone quiet is down, not yet heard from is stopped. */
export function serverStatus(server: Server): Status {
  return STATUS[server.connection];
}

/** What the shell needs to know about the fleet's size. */
export interface FleetShape {
  /** Every server, or undefined until the first snapshot arrives. */
  servers: Server[] | undefined;
  /** True from the second server on. A single-server user never sees fleet concepts before then. */
  fleet: boolean;
  /** The one server, while there is exactly one. */
  only: Server | undefined;
}

export function fleetShape(state: FleetState): FleetShape {
  const servers = state.status === "connecting" ? undefined : state.servers;
  return {
    servers,
    fleet: (servers?.length ?? 0) > 1,
    only: servers?.length === 1 ? servers[0] : undefined,
  };
}

/** Whether ip is in a private, shared or unique-local range, so not reachable from the internet. */
export function isPrivateAddress(ip: string): boolean {
  if (ip.includes(":")) return /^f[cd]/i.test(ip);
  const [a = 0, b = 0] = ip.split(".").map(Number);
  return (
    a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) || (a === 100 && b >= 64 && b <= 127)
  );
}

/** The address people reach a server on: its first public IPv4, then public IPv6, then whatever it has. */
export function primaryAddress(addresses: readonly string[]): string | undefined {
  const isPublic = addresses.filter((ip) => !isPrivateAddress(ip));
  return isPublic.find((ip) => !ip.includes(":")) ?? isPublic[0] ?? addresses[0];
}
