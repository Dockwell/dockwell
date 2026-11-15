// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { GitHubCreated } from "../../../features/settings/GitHubReturn";

interface CreatedSearch {
  code: string;
  state: string;
}

/** Where GitHub sends the browser once it made the GitHub App (ADR 0098). */
export const Route = createFileRoute("/_shell/_settings/settings/integrations/github/created")({
  validateSearch: (search: Record<string, unknown>): CreatedSearch => ({
    code: typeof search["code"] === "string" ? search["code"] : "",
    state: typeof search["state"] === "string" ? search["state"] : "",
  }),
  component: function Created() {
    const { code, state } = Route.useSearch();
    return <GitHubCreated code={code} state={state} />;
  },
});
