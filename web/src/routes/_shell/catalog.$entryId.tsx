// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { CatalogPage } from "../../features/catalog/CatalogPage";
import { InstallSheet } from "../../features/catalog/InstallSheet";

/** Installing an app: its sheet over the catalog, as Catalog-Install draws it, at an address of its own. */
export const Route = createFileRoute("/_shell/catalog/$entryId")({
  component: function InstallApp() {
    const { entryId } = Route.useParams();
    const navigate = useNavigate();
    return (
      <CatalogPage>
        <InstallSheet id={entryId} onClose={() => void navigate({ to: "/catalog" })} />
      </CatalogPage>
    );
  },
});
