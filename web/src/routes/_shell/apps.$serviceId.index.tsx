// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AppOverview } from "../../features/apps/AppOverview";

export const Route = createFileRoute("/_shell/apps/$serviceId/")({
  component: function Overview() {
    const { serviceId } = Route.useParams();
    return <AppOverview serviceId={serviceId} />;
  },
});
