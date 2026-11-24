// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { DatabasesPage, type DatabasesSearch } from "../../features/databases/DatabasesPage";
import { readListSearch } from "../../features/shell/list";

/** Reads where the list is and the engine a new database starts from, leaving out what isn't one. */
function readSearch(search: Record<string, unknown>): DatabasesSearch {
  const value = search["new"];
  const engine = search["engine"];
  return {
    ...readListSearch(search),
    ...(typeof value === "string" && value !== "" ? { new: value } : {}),
    ...(typeof engine === "string" && engine !== "" ? { engine } : {}),
  };
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
