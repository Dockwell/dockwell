// SPDX-License-Identifier: AGPL-3.0-only

import { createLazyFileRoute } from "@tanstack/react-router";
import { DockerWorkspace } from "../../features/docker/DockerWorkspace";

export const Route = createLazyFileRoute("/_shell/servers/$serverId/docker/volumes/")({
  component: function Volumes() {
    return <DockerWorkspace serverId={Route.useParams().serverId} tab="volumes" />;
  },
});
