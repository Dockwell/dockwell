// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { WebsitesPage } from "../../features/websites/WebsitesPage";

export const Route = createFileRoute("/_shell/websites/")({
  component: function Websites() {
    return <WebsitesPage />;
  },
});
