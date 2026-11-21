// SPDX-License-Identifier: AGPL-3.0-only

import { createLazyFileRoute } from "@tanstack/react-router";
import { DockerNetworkDetail } from "../../features/docker/DockerNetworkDetail";

export const Route = createLazyFileRoute("/_shell/servers/$serverId/docker/networks/$networkId")({
  component: function Network() {
    return <DockerNetworkDetail {...Route.useParams()} />;
  },
});
