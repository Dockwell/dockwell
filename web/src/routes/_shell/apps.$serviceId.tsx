// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, Outlet } from "@tanstack/react-router";

import { AppLayout } from "../../features/apps/AppLayout";

/** One app: its head and tabs, around the tab's own route. */
export const Route = createFileRoute("/_shell/apps/$serviceId")({
  component: function App() {
    const { serviceId } = Route.useParams();
    return (
      <AppLayout serviceId={serviceId}>
        <Outlet />
      </AppLayout>
    );
  },
});
