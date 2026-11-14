// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useCallback } from "react";

import { FleetActionPage, type FleetActionSearch } from "../../features/fleet/FleetActionPage";

/** Reads the servers picked on Servers, or a selector of their labels, from the URL, leaving out anything else. */
function readSearch(search: Record<string, unknown>): FleetActionSearch {
  const servers = search["servers"];
  if (typeof servers === "string") {
    const ids = servers
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    if (ids.length > 0) return { servers: ids.join(",") };
  }
  const selector = search["selector"];
  return typeof selector === "string" && selector.trim() !== "" ? { selector } : {};
}

export const Route = createFileRoute("/_shell/fleet-actions")({
  validateSearch: readSearch,
  component: function FleetActions() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    const onSearch = useCallback(
      (next: FleetActionSearch, how?: { replace?: boolean }) =>
        void navigate({
          to: "/fleet-actions",
          search: next,
          replace: how?.replace ?? false,
          resetScroll: false,
        }),
      [navigate],
    );
    return <FleetActionPage search={search} onSearch={onSearch} />;
  },
});
