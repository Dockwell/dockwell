// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { InstallSheet } from "../../features/catalog/InstallSheet";

export const Route = createFileRoute("/_shell/catalog/$entryId/install")({
  component: function InstallCatalogApp() {
    const { entryId } = Route.useParams();
    const navigate = useNavigate();
    return (
      <InstallSheet
        id={entryId}
        onClose={() => void navigate({ to: "/catalog/$entryId", params: { entryId }, search: (previous) => previous })}
      />
    );
  },
});
