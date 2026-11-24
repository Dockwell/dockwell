// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";
import { DockerWorkspace } from "../../features/docker/DockerWorkspace";
import { readServerCleanupSearch } from "../../features/docker/search";

export const Route = createFileRoute("/_shell/servers/$serverId/docker/cleanup")({
  validateSearch: readServerCleanupSearch,
  component: function Cleanup() {
    const search = Route.useSearch();
    return (
      <DockerWorkspace
        serverId={Route.useParams().serverId}
        tab="cleanup"
        group={search.group}
        cleanupKinds={search.kinds}
      />
    );
  },
});
