// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { AppsPage, type AppsSearch } from "../../features/apps/AppsPage";
import { readListSearch } from "../../features/shell/list";

/** Reads the list's filters and where it is in them from its URL, leaving out what isn't one. */
function readSearch(search: Record<string, unknown>): AppsSearch {
  const out: AppsSearch = { ...readListSearch(search) };
  for (const key of ["environment", "server", "show"] as const) {
    const value = search[key];
    if (typeof value === "string" && value !== "") out[key] = value;
  }
  return out;
}

/** Without the filters left unset, for its URL. */
function without(search: AppsSearch): AppsSearch {
  return Object.fromEntries(Object.entries(search).filter(([, v]) => v !== undefined)) as AppsSearch;
}

export const Route = createFileRoute("/_shell/apps/")({
  validateSearch: readSearch,
  component: function Apps() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <AppsPage
        search={search}
        onSearch={(next) => void navigate({ to: "/apps", search: without(next), replace: true })}
      />
    );
  },
});
