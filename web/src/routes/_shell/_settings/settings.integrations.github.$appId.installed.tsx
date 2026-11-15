// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { GitHubInstalled } from "../../../features/settings/GitHubReturn";

interface InstalledSearch {
  installation_id: string;
  setup_action: string;
}

/** Where GitHub sends the browser once the GitHub App is installed on an account (ADR 0098). */
export const Route = createFileRoute("/_shell/_settings/settings/integrations/github/$appId/installed")({
  validateSearch: (search: Record<string, unknown>): InstalledSearch => ({
    installation_id: search["installation_id"] === undefined ? "" : String(search["installation_id"]),
    setup_action: typeof search["setup_action"] === "string" ? search["setup_action"] : "",
  }),
  component: function Installed() {
    const { appId } = Route.useParams();
    const search = Route.useSearch();
    return <GitHubInstalled appId={appId} installationId={search.installation_id} action={search.setup_action} />;
  },
});
