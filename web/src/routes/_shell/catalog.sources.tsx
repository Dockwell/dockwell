// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { SourcesPage } from "../../features/catalog/SourcesPage";
import { readListSearch } from "../../features/shell/list";

export const Route = createFileRoute("/_shell/catalog/sources")({
  validateSearch: readListSearch,
  component: function Sources() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <SourcesPage
        search={search}
        onSearch={(next) => void navigate({ to: "/catalog/sources", search: next, replace: true, resetScroll: false })}
      />
    );
  },
});
