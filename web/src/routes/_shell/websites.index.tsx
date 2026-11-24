// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { readListSearch } from "../../features/shell/list";
import { WebsitesPage, type WebsitesSearch } from "../../features/websites/WebsitesPage";

/** Reads where the list is and the kind it shows, leaving out what isn't one. */
function readSearch(search: Record<string, unknown>): WebsitesSearch {
  const kind = search["kind"];
  return {
    ...readListSearch(search),
    ...(kind === "folder" || kind === "php" || kind === "proxy" || kind === "unknown" ? { kind } : {}),
  };
}

export const Route = createFileRoute("/_shell/websites/")({
  validateSearch: readSearch,
  component: function Websites() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <WebsitesPage
        search={search}
        onSearch={(next) => void navigate({ to: "/websites", search: next, replace: true })}
      />
    );
  },
});
