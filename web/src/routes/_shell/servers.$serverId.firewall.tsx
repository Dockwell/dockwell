// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { FirewallPage } from "../../features/servers/FirewallPage";

/** What filters a server's inbound traffic. */
export const Route = createFileRoute("/_shell/servers/$serverId/firewall")({
  component: function Firewall() {
    const { serverId } = Route.useParams();
    return <FirewallPage id={serverId} />;
  },
});
