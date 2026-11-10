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
  const readable = server.connection === Connection.CONNECTED && server.dockerUnreadableSince === undefined;
  // The same read as the containers panel's, which the query cache shares.
  // A server's containers are an admin's to read, and nobody else is asked to move them (ADR 0059).
  const allowed = useMay()(ContainerService.method.listContainers).allowed;
  const list = useQuery(
    ContainerService.method.listContainers,
    { server: server.id },
    { retry: false, enabled: readable && allowed },
  );
  return readable && (list.data?.containers.some((c) => leftToMove(c.name)) ?? false);
}
