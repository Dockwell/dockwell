// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";
import { DockerWorkspace } from "../../features/docker/DockerWorkspace";

export const Route = createFileRoute("/_shell/servers/$serverId/docker/cleanup")({
  validateSearch: (search: Record<string, unknown>) => ({
    ...(typeof search["group"] === "string" ? { group: search["group"] } : {}),
  }),
  component: function Cleanup() {
    return <DockerWorkspace serverId={Route.useParams().serverId} tab="cleanup" group={Route.useSearch().group} />;
  },
});
