// SPDX-License-Identifier: AGPL-3.0-only

import type { Server } from "@dockwell/api";

/**
 * The commands a server's own service manager takes for Dockwell's agent and for
 * Docker, as install.sh sets them up: OpenRC on Alpine, systemd everywhere else.
 * When the agent can't be reached, Dockwell can't act on the server, so these are
 * what the panel offers to copy, in place of a sentence that says what to check.
 */
export function serviceCommands(server: Pick<Server, "inventory">): {
  restartAgent: string;
  startAgent: string;
  restartDocker: string;
} {
  const openrc = server.inventory?.osId === "alpine";
  return openrc
    ? {
        restartAgent: "sudo rc-service dockwell restart",
        startAgent: "sudo rc-service dockwell start",
        restartDocker: "sudo rc-service docker restart",
      }
    : {
        restartAgent: "sudo systemctl restart dockwell",
        startAgent: "sudo systemctl enable --now dockwell",
        restartDocker: "sudo systemctl restart docker",
      };
}
