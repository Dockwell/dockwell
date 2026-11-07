// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { TerminalPage } from "../../features/terminal/TerminalPage";

/** A server's terminal: root's shell, through its agent. */
export const Route = createFileRoute("/_shell/servers/$serverId/terminal")({
  component: function Terminal() {
    const { serverId } = Route.useParams();
    return <TerminalPage serverId={serverId} />;
  },
});
