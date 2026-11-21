// SPDX-License-Identifier: AGPL-3.0-only

import { createLazyFileRoute } from "@tanstack/react-router";

import { ComposeStack } from "../../features/docker/ComposeStack";

export const Route = createLazyFileRoute("/_shell/servers/$serverId/docker/composes/$composeId")({
  component: function ComposeDetail() {
    const navigate = Route.useNavigate();
    return (
      <ComposeStack
        {...Route.useParams()}
        search={Route.useSearch()}
        onSearch={(next) => void navigate({ search: next, replace: true })}
      />
    );
  },
});
