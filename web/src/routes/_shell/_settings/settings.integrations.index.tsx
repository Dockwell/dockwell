// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { IntegrationsPage } from "../../../features/settings/IntegrationsPage";
import { readListSearch } from "../../../features/shell/list";

/** What Dockwell connects to: storage backups go to, and what routes sit behind. */
export const Route = createFileRoute("/_shell/_settings/settings/integrations/")({
  validateSearch: readListSearch,
  component: function Integrations() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <IntegrationsPage
        search={search}
        onSearch={(next) =>
          void navigate({
            to: "/settings/integrations",
            search: next,
            replace: true,
            resetScroll: false,
          })
        }
      />
    );
  },
});
