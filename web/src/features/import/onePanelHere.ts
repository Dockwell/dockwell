// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { ContainerService } from "@dockwell/api";

/**
 * Whether a container is one 1Panel made that's left to move: 1Panel names each
 * after itself, and OpenResty stays 1Panel's until websites move. One Dockwell took
 * on isn't in the list of what it didn't make.
 */
export function leftToMove(name: string): boolean {
  return name.startsWith("1Panel-") && !name.startsWith("1Panel-openresty-");
}

/** Whether 1Panel's containers that are left to move run on a server, as its list says. */
export function useOnePanelHere(serverId: string): boolean {
  // The same read as the containers panel's, which the query cache shares.
  const list = useQuery(ContainerService.method.listContainers, { server: serverId }, { retry: false });
  return list.data?.containers.some((c) => leftToMove(c.name)) ?? false;
}
