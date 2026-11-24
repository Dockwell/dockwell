// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { type AlertsSearch, AlertsTab } from "../../features/alerts/ListTabs";
import { readListSearch } from "../../features/shell/list";

/** What's firing now, and what fired before. */
export const Route = createFileRoute("/_shell/alerts/")({
  validateSearch: (search: Record<string, unknown>): AlertsSearch => ({
    ...readListSearch(search),
    ...(search["view"] === "open" || search["view"] === "resolved" ? { view: search["view"] } : {}),
  }),
  component: function AlertsTabRoute() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <AlertsTab
        search={search}
        onSearch={(next) => void navigate({ to: "/alerts", search: next, replace: true, resetScroll: false })}
      />
    );
  },
});
