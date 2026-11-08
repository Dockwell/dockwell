// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { UpdatesPage } from "../../features/updates/UpdatesPage";

/** A server's package updates: what waits, which are security fixes, and whether it needs a reboot. */
export const Route = createFileRoute("/_shell/servers/$serverId/updates")({
  component: function Updates() {
    const { serverId } = Route.useParams();
    return <UpdatesPage serverId={serverId} />;
  },
});
