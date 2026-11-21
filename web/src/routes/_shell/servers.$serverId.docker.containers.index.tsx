// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";
import { DockerWorkspace } from "../../features/docker/DockerWorkspace";

export const Route = createFileRoute("/_shell/servers/$serverId/docker/containers/")({
  validateSearch: (search: Record<string, unknown>): { attention?: boolean } =>
    search["attention"] === true ? { attention: true } : {},
  component: function ContainerInventory() {
    return (
      <DockerWorkspace serverId={Route.useParams().serverId} tab="containers" attention={Route.useSearch().attention} />
    );
  },
});
