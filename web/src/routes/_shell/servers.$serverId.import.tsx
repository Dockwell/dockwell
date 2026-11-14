// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { MoveInPage } from "../../features/import/PanelImport";

/** Moving a server off 1Panel, Coolify or Dokploy: what each runs, and what Dockwell would take on, make again and leave. */
export const Route = createFileRoute("/_shell/servers/$serverId/import")({
  component: function Import() {
    const { serverId } = Route.useParams();
    return <MoveInPage serverId={serverId} />;
  },
});
