// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { readLogsSearch } from "../../features/logs/LogsPage";
import { ContainerPage } from "../../features/servers/ContainerPage";

/** A container Dockwell didn't make, with what it prints: its view of the logs is in its URL, as an app's is. */
export const Route = createFileRoute("/_shell/servers/$serverId/containers/$name")({
  validateSearch: readLogsSearch,
  component: function Container() {
    const { serverId, name } = Route.useParams();
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <ContainerPage
        serverId={serverId}
        name={name}
        search={search}
        onSearch={(next) =>
          void navigate({
            to: "/servers/$serverId/containers/$name",
            params: { serverId, name },
            search: next,
            replace: true,
          })
        }
      />
    );
  },
});
