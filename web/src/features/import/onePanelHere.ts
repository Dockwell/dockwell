// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { Connection, ContainerService, type Server } from "@dockwell/api";

import { useMay } from "../auth/may";

/**
 * Whether a container is one 1Panel made that's left to move: 1Panel names each
 * after itself, and OpenResty stays 1Panel's until websites move. One Dockwell took
 * on isn't in the list of what it didn't make.
 */
export function leftToMove(name: string): boolean {
  return name.startsWith("1Panel-") && !name.startsWith("1Panel-openresty-");
}

/** Whether 1Panel's containers that are left to move run on a server, as its list says. */
export function useOnePanelHere(server: Server): boolean {
  return useOnePanelLeft(server).length > 0;
}

/**
 * What of 1Panel's is left to move on a server, by the app each container is of, as
 * 1Panel names them, "1Panel-<app>-<suffix>": each app once, in the list's order.
 */
export function useOnePanelLeft(server: Server): string[] {
  const readable = server.connection === Connection.CONNECTED && server.dockerUnreadableSince === undefined;
  // The same read as the containers panel's, which the query cache shares.
  // A server's containers are an admin's to read, and nobody else is asked to move them (ADR 0059).
  const allowed = useMay()(ContainerService.method.listContainers).allowed;
  const list = useQuery(
    ContainerService.method.listContainers,
    { server: server.id },
    { retry: false, enabled: readable && allowed },
  );
  if (!readable) return [];
  const apps = (list.data?.containers ?? []).filter((c) => leftToMove(c.name)).map((c) => appOf1Panel(c.name));
  return [...new Set(apps)];
}

/** The app a 1Panel container is of: "1Panel-mysql-Xk2p" is mysql's. */
export function appOf1Panel(name: string): string {
  const rest = name.slice("1Panel-".length);
  const cut = rest.lastIndexOf("-");
  return cut > 0 ? rest.slice(0, cut) : rest;
}
