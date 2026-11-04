// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ServerOverview } from "../../features/servers/ServerOverview";

export const Route = createFileRoute("/_shell/servers/$serverId")({
  component: function Server() {
    const { serverId } = Route.useParams();
    return <ServerOverview id={serverId} />;
  },
});
