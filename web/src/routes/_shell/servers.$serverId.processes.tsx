// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ProcessesPage } from "../../features/servers/ProcessesPage";
import { readListSearch } from "../../features/shell/list";

/** What runs on a server, process by process. */
export const Route = createFileRoute("/_shell/servers/$serverId/processes")({
  validateSearch: readListSearch,
  component: function Processes() {
    const { serverId } = Route.useParams();
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <ProcessesPage
        serverId={serverId}
        search={search}
        onSearch={(next) =>
          void navigate({
            to: "/servers/$serverId/processes",
            params: { serverId },
            search: next,
            replace: true,
            resetScroll: false,
          })
        }
      />
    );
  },
});
