// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ProcessesPage } from "../../features/servers/ProcessesPage";

/** What runs on a server, process by process. */
export const Route = createFileRoute("/_shell/servers/$serverId/processes")({
  component: function Processes() {
    const { serverId } = Route.useParams();
    return <ProcessesPage serverId={serverId} />;
  },
});
