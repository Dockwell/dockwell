// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { readLogsSearch } from "../../features/logs/LogsPage";
import { WebsiteLogs } from "../../features/websites/WebsiteLogs";

/** A website's logs: its web server's, following live, a time range, or one deploy's. */
export const Route = createFileRoute("/_shell/websites/$websiteId/logs")({
  validateSearch: readLogsSearch,
  component: function Logs() {
    const { websiteId } = Route.useParams();
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <WebsiteLogs
        search={search}
        onSearch={(next) =>
          void navigate({ to: "/websites/$websiteId/logs", params: { websiteId }, search: next, replace: true })
        }
      />
    );
  },
});
