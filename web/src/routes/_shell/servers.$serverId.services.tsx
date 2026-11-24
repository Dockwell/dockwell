// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { SystemServicesPage, type SystemServicesSearch } from "../../features/servers/SystemServicesPage";
import { readListSearch } from "../../features/shell/list";

/** A server's system services: its systemd units or OpenRC services (ADR 0067). */
export const Route = createFileRoute("/_shell/servers/$serverId/services")({
  validateSearch: (search: Record<string, unknown>): SystemServicesSearch => ({
    ...readListSearch(search),
    ...(typeof search["unit"] === "string" && /^[\w@.:-]{1,255}$/.test(search["unit"]) ? { unit: search["unit"] } : {}),
  }),
  component: function SystemServices() {
    const { serverId } = Route.useParams();
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <SystemServicesPage
        key={`${serverId}:${search.unit ?? ""}`}
        serverId={serverId}
        search={search}
        onSearch={(next) =>
          void navigate({
            to: "/servers/$serverId/services",
            params: { serverId },
            search: next,
            replace: true,
            resetScroll: false,
          })
        }
      />
    );
  },
});
