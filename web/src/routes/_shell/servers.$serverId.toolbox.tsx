// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";
import { ToolboxPage } from "../../features/servers/ToolboxPage";

export const Route = createFileRoute("/_shell/servers/$serverId/toolbox")({
  component: function Toolbox() {
    const { serverId } = Route.useParams();
    return <ToolboxPage key={serverId} serverId={serverId} />;
  },
});
