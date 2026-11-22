// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { SystemServicesPage } from "../../features/servers/SystemServicesPage";

/** A server's system services: its systemd units or OpenRC services (ADR 0067). */
export const Route = createFileRoute("/_shell/servers/$serverId/services")({
  validateSearch: (search: Record<string, unknown>): { unit?: string } =>
    typeof search["unit"] === "string" && /^[\w@.:-]{1,255}$/.test(search["unit"]) ? { unit: search["unit"] } : {},
  component: function SystemServices() {
    const { serverId } = Route.useParams();
    const { unit } = Route.useSearch();
    return <SystemServicesPage key={`${serverId}:${unit ?? ""}`} serverId={serverId} unit={unit} />;
  },
});
