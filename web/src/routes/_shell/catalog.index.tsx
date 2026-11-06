// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { CatalogPage, type CatalogSearch } from "../../features/catalog/CatalogPage";

/** Reads the catalog's filters from its URL, leaving out what isn't one. */
function readSearch(search: Record<string, unknown>): CatalogSearch {
  const out: CatalogSearch = {};
  for (const key of ["q", "category"] as const) {
    const value = search[key];
    if (typeof value === "string" && value !== "") out[key] = value;
  }
  return out;
}

/** Without what's left unset, for its URL. */
function without(search: CatalogSearch): CatalogSearch {
  return Object.fromEntries(Object.entries(search).filter(([, v]) => v !== undefined && v !== "")) as CatalogSearch;
}

export const Route = createFileRoute("/_shell/catalog/")({
  validateSearch: readSearch,
  component: function Catalog() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <CatalogPage
        search={search}
        onSearch={(next) => void navigate({ to: "/catalog", search: without(next), replace: true })}
      />
    );
  },
});
