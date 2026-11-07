// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, Outlet } from "@tanstack/react-router";

import { WebsiteLayout } from "../../features/websites/WebsiteLayout";

/** One website: its head and tabs, around the tab's own route. */
export const Route = createFileRoute("/_shell/websites/$websiteId")({
  component: function Website() {
    const { websiteId } = Route.useParams();
    return (
      <WebsiteLayout websiteId={websiteId}>
        <Outlet />
      </WebsiteLayout>
    );
  },
});
