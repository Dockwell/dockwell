// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { ServersPage, type ServersSearch } from "../../features/servers/ServersPage";

/** Reads which server is inspected from the URL, leaving out anything else. */
function readSearch(search: Record<string, unknown>): ServersSearch {
  const server = search["server"];
  return typeof server === "string" && server !== "" ? { server } : {};
}

export const Route = createFileRoute("/_shell/servers/")({
  validateSearch: readSearch,
  component: function Servers() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <ServersPage
        search={search}
        onSearch={(next) => void navigate({ to: "/servers", search: next, replace: true })}
      />
    );
  },
});
