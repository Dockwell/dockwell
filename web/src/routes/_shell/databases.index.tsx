// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { DatabasesPage, type DatabasesSearch } from "../../features/databases/DatabasesPage";

/** Reads the engine a new database starts from, leaving out what isn't one. */
function readSearch(search: Record<string, unknown>): DatabasesSearch {
  const value = search["new"];
  return typeof value === "string" && value !== "" ? { new: value } : {};
}

/** Every database, with how each is, the way to make another, and their backups. */
export const Route = createFileRoute("/_shell/databases/")({
  validateSearch: readSearch,
  component: function Databases() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <DatabasesPage
        search={search}
        onSearch={(next) => void navigate({ to: "/databases", search: next, replace: true })}
      />
    );
  },
});
