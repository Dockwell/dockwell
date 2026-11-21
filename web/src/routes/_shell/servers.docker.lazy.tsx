// SPDX-License-Identifier: AGPL-3.0-only

import { createLazyFileRoute } from "@tanstack/react-router";

import { DockerWorkspace } from "../../features/docker/DockerWorkspace";

export const Route = createLazyFileRoute("/_shell/servers/docker")({
  component: function FleetDocker() {
    return <DockerWorkspace tab={Route.useSearch().view ?? "overview"} attention={Route.useSearch().attention} />;
  },
});
