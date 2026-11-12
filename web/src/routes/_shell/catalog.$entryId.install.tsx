// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { InstallSheet } from "../../features/catalog/InstallSheet";

export const Route = createFileRoute("/_shell/catalog/$entryId/install")({
  component: function InstallCatalogApp() {
    const { entryId } = Route.useParams();
    const { catalog } = Route.useSearch();
    const navigate = useNavigate();
    return (
      <InstallSheet
        id={entryId}
        catalog={catalog ?? ""}
        onClose={() => void navigate({ to: "/catalog/$entryId", params: { entryId }, search: (previous) => previous })}
      />
    );
  },
});
