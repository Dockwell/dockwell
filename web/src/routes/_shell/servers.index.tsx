// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useCallback } from "react";

import { ServersPage, type ServersSearch } from "../../features/servers/ServersPage";

/**
 * Reads whether Add a server opens with the page, which server is inspected, or that
 * none is, and the selector and grouping, from the URL, leaving out anything else.
 */
function readSearch(search: Record<string, unknown>): ServersSearch {
  if (search["add"] === true || search["add"] === 1 || search["add"] === "1") return { add: true };
  const out: ServersSearch = {};
  const selector = search["selector"];
  if (typeof selector === "string" && selector.trim() !== "") out.selector = selector;
  const group = search["group"];
  if (typeof group === "string" && group !== "") out.group = group;
  const server = search["server"];
  if (typeof server === "string" && server !== "") return { ...out, server };
  const closed = search["closed"];
  return closed === true || closed === "true" || closed === 1 ? { ...out, closed: true } : out;
}

export const Route = createFileRoute("/_shell/servers/")({
  validateSearch: readSearch,
  component: function Servers() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    // Choosing a server keeps the page where it is: a reader far down the list stays there.
    const onSearch = useCallback(
      (next: ServersSearch, how?: { replace?: boolean }) =>
        void navigate({ to: "/servers", search: next, replace: how?.replace ?? false, resetScroll: false }),
      [navigate],
    );
    return <ServersPage search={search} onSearch={onSearch} />;
  },
});
