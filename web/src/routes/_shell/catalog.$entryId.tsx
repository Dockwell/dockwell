// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, Outlet } from "@tanstack/react-router";

import { CatalogDetailPage } from "../../features/catalog/CatalogDetailPage";
import { catalogSearch } from "../../features/catalog/search";

/** An app's page comes before its separate installation form. */
export const Route = createFileRoute("/_shell/catalog/$entryId")({
  validateSearch: catalogSearch,
  component: function CatalogApp() {
    const { entryId } = Route.useParams();
    const search = Route.useSearch();
    return (
      <CatalogDetailPage id={entryId} search={search}>
        <Outlet />
      </CatalogDetailPage>
    );
  },
});
