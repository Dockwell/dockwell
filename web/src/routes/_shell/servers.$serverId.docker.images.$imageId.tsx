// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";
import { DockerImageDetail } from "../../features/docker/DockerImages";

export const Route = createFileRoute("/_shell/servers/$serverId/docker/images/$imageId")({
  component: function Image() {
    return <DockerImageDetail {...Route.useParams()} />;
  },
});
