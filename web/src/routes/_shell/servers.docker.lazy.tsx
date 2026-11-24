// SPDX-License-Identifier: AGPL-3.0-only

import { createLazyFileRoute } from "@tanstack/react-router";

import { DockerWorkspace } from "../../features/docker/DockerWorkspace";

export const Route = createLazyFileRoute("/_shell/servers/docker")({
  component: function FleetDocker() {
    const search = Route.useSearch();
    return (
      <DockerWorkspace
        tab={search.view ?? "overview"}
        attention={search.attention}
        cleanupKinds={search.kinds}
        cleanupServerIds={search.servers}
      />
    );
  },
});
