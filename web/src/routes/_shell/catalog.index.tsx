// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { CatalogPage } from "../../features/catalog/CatalogPage";
import { catalogSearch } from "../../features/catalog/search";

export const Route = createFileRoute("/_shell/catalog/")({
  validateSearch: catalogSearch,
  component: function Catalog() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <CatalogPage
        search={search}
        onSearch={(next) => void navigate({ to: "/catalog", search: catalogSearch({ ...next }), replace: true })}
      />
    );
  },
});
