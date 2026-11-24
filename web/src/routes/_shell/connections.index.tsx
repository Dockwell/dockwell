// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { ConnectionsPage, type ConnectionsSearch } from "../../features/connections/ConnectionsPage";
import { readListSearch } from "../../features/shell/list";

/** Every connection: the buckets apps keep files in, and the way to add one (ADR 0099). */
export const Route = createFileRoute("/_shell/connections/")({
  validateSearch: (search: Record<string, unknown>): ConnectionsSearch => {
    const kind = search["kind"];
    return {
      ...readListSearch(search),
      ...(kind === "mail" || kind === "storage" || kind === "destination" ? { kind } : {}),
    };
  },
  component: function Connections() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <ConnectionsPage
        search={search}
        onSearch={(next) => void navigate({ to: "/connections", search: next, replace: true })}
      />
    );
  },
});
