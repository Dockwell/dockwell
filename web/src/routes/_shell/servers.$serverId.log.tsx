// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { SystemLogPage } from "../../features/servers/SystemLogPage";
import { readSystemLogSearch } from "../../features/servers/systemLog";

/** A server's system log: its journal, followed live or read back, narrowed (ADR 0067). */
export const Route = createFileRoute("/_shell/servers/$serverId/log")({
  validateSearch: readSystemLogSearch,
  component: function SystemLog() {
    const { serverId } = Route.useParams();
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <SystemLogPage
        serverId={serverId}
        search={search}
        onSearch={(next) =>
          void navigate({ to: "/servers/$serverId/log", params: { serverId }, search: next, replace: true })
        }
      />
    );
  },
});
