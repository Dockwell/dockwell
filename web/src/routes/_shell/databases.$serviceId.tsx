// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, Outlet } from "@tanstack/react-router";

import { DatabaseLayout } from "../../features/databases/DatabaseLayout";

/** One database: its head and tabs, around the tab's own route. */
export const Route = createFileRoute("/_shell/databases/$serviceId")({
  component: function Database() {
    const { serviceId } = Route.useParams();
    return (
      <DatabaseLayout serviceId={serviceId}>
        <Outlet />
      </DatabaseLayout>
    );
  },
});
