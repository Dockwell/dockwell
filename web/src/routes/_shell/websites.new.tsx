// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { NewWebsiteSheet } from "../../features/websites/NewWebsiteSheet";
import { WebsitesPage } from "../../features/websites/WebsitesPage";

interface NewWebsiteSearch {
  /** PHP, of a version, as a runtime's card starts it. */
  php?: string;
}

/** New website: its sheet over the list, at an address of its own, as New app has. */
export const Route = createFileRoute("/_shell/websites/new")({
  // The router reads ?php=8.4 typed by hand as a number, and writes its own quoted.
  validateSearch: (search: Record<string, unknown>): NewWebsiteSearch => {
    const php = typeof search["php"] === "number" ? search["php"].toFixed(1) : search["php"];
    return typeof php === "string" && /^\d+\.\d+$/.test(php) ? { php } : {};
  },
  component: function NewWebsite() {
    const { php } = Route.useSearch();
    const navigate = useNavigate();
    return (
      <WebsitesPage>
        <NewWebsiteSheet
          start={php ? { serves: "php", version: php } : undefined}
          onClose={() => void navigate({ to: "/websites" })}
        />
      </WebsitesPage>
    );
  },
});
