// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ServerOverview } from "../../features/servers/ServerOverview";
import { readListSearch } from "../../features/shell/list";

export const Route = createFileRoute("/_shell/servers/$serverId/")({
  validateSearch: readListSearch,
  component: function Server() {
    const { serverId } = Route.useParams();
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <ServerOverview
        id={serverId}
        search={search}
        onSearch={(next) =>
          void navigate({
            to: "/servers/$serverId",
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
