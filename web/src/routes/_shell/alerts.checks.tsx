// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { ChecksTab } from "../../features/alerts/ChecksTab";
import { readListSearch } from "../../features/shell/list";

/** The addresses Dockwell asks, how each stands, and its last answers. */
export const Route = createFileRoute("/_shell/alerts/checks")({
  validateSearch: readListSearch,
  component: function ChecksTabRoute() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <ChecksTab
        search={search}
        onSearch={(next) => void navigate({ to: "/alerts/checks", search: next, replace: true, resetScroll: false })}
      />
    );
  },
});
