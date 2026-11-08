// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { OnePanelImportPage } from "../../features/import/OnePanelImport";

/** Moving a server off 1Panel: what it keeps, and what Dockwell would take on, make again and leave. */
export const Route = createFileRoute("/_shell/servers/$serverId/import")({
  component: function Import() {
    const { serverId } = Route.useParams();
    return <OnePanelImportPage serverId={serverId} />;
  },
});
