// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";
import { DockerWorkspace } from "../../features/docker/DockerWorkspace";

export const Route = createFileRoute("/_shell/servers/$serverId/docker/images/")({
  component: function Images() {
    return <DockerWorkspace serverId={Route.useParams().serverId} tab="images" />;
  },
});
