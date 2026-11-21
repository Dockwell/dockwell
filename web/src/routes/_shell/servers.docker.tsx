// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";
import { DockerWorkspace } from "../../features/docker/DockerWorkspace";

export const Route = createFileRoute("/_shell/servers/docker")({
  validateSearch: (search: Record<string, unknown>): { view?: "containers"; attention?: boolean } =>
    search["view"] === "containers"
      ? { view: "containers", ...(search["attention"] === true ? { attention: true } : {}) }
      : {},
  component: function FleetDocker() {
    return <DockerWorkspace tab={Route.useSearch().view ?? "overview"} attention={Route.useSearch().attention} />;
  },
});
