// SPDX-License-Identifier: AGPL-3.0-only

import { createLazyFileRoute } from "@tanstack/react-router";
import { DockerVolumeDetail } from "../../features/docker/DockerVolumeDetail";

export const Route = createLazyFileRoute("/_shell/servers/$serverId/docker/volumes/$volumeKey")({
  component: function Volume() {
    return <DockerVolumeDetail {...Route.useParams()} />;
  },
});
