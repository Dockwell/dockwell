// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { NewWebsiteSheet } from "../../features/websites/NewWebsiteSheet";
import { WebsitesPage } from "../../features/websites/WebsitesPage";

/** New website: its sheet over the list, at an address of its own, as New app has. */
export const Route = createFileRoute("/_shell/websites/new")({
  component: function NewWebsite() {
    const navigate = useNavigate();
    return (
      <WebsitesPage>
        <NewWebsiteSheet onClose={() => void navigate({ to: "/websites" })} />
      </WebsitesPage>
    );
  },
});
