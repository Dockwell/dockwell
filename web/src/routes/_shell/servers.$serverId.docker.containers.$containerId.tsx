// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";
import { DockerContainerDetail } from "../../features/docker/DockerContainerDetail";
import { readContainerDetailSearch } from "../../features/docker/search";

export const Route = createFileRoute("/_shell/servers/$serverId/docker/containers/$containerId")({
  validateSearch: readContainerDetailSearch,
  component: function ContainerDetail() {
    const params = Route.useParams();
    const search = Route.useSearch();
    const navigate = Route.useNavigate();
    return (
      <DockerContainerDetail
        {...params}
        search={search}
        onSearch={(next) => void navigate({ search: next, replace: true })}
      />
    );
  },
});
