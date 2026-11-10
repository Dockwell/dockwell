// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { SystemServicesPage } from "../../features/servers/SystemServicesPage";

/** A server's system services: its systemd units or OpenRC services (ADR 0067). */
export const Route = createFileRoute("/_shell/servers/$serverId/services")({
  component: function SystemServices() {
    const { serverId } = Route.useParams();
    return <SystemServicesPage serverId={serverId} />;
  },
});
