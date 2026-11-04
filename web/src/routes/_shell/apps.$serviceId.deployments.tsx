// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AppDeployments } from "../../features/apps/AppDeployments";

export const Route = createFileRoute("/_shell/apps/$serviceId/deployments")({
  component: function Deployments() {
    const { serviceId } = Route.useParams();
    return <AppDeployments serviceId={serviceId} />;
  },
});
