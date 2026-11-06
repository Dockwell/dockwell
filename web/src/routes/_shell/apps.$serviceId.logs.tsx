// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { LogsPage, readLogsSearch } from "../../features/logs/LogsPage";

/** An app's logs: following live, a time range, or one deploy's. */
export const Route = createFileRoute("/_shell/apps/$serviceId/logs")({
  validateSearch: readLogsSearch,
  component: function Logs() {
    const { serviceId } = Route.useParams();
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <LogsPage
        id={serviceId}
        search={search}
        onSearch={(next) =>
          void navigate({ to: "/apps/$serviceId/logs", params: { serviceId }, search: next, replace: true })
        }
      />
    );
  },
});
