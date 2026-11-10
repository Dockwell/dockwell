// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { type LogsSearch, readLogsSearch } from "../../features/logs/LogsPage";
import { WebsiteLogs, type Which } from "../../features/websites/WebsiteLogs";

type WebsiteLogsSearch = LogsSearch & { which?: Which };

/** A website's logs: its web server's or its PHP's, following live, a time range, or one deploy's. */
export const Route = createFileRoute("/_shell/websites/$websiteId/logs")({
  validateSearch: (search: Record<string, unknown>): WebsiteLogsSearch => ({
    ...readLogsSearch(search),
    ...(search["which"] === "php" ? { which: "php" as const } : {}),
  }),
  component: function Logs() {
    const { websiteId } = Route.useParams();
    const { which, ...search } = Route.useSearch();
    const navigate = useNavigate();
    const go = (next: WebsiteLogsSearch) =>
      void navigate({ to: "/websites/$websiteId/logs", params: { websiteId }, search: next, replace: true });
    return (
      <WebsiteLogs
        which={which ?? "web"}
        search={search}
        onSearch={(next) => go({ ...next, ...(which ? { which } : {}) })}
        onWhich={(next) => go(next === "php" ? { which: "php" } : {})}
      />
    );
  },
});
