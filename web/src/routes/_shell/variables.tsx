// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { VariablesPage } from "../../features/variables/VariablesPage";

interface VariablesSearch {
  environment?: string;
  q?: string;
  /** An app, by ID: only what it runs with. */
  app?: string;
}

/**
 * An environment's variables and secrets, from the rail (ADR 0074). An
 * app's Environment tab shows its own share, and links here.
 */
export const Route = createFileRoute("/_shell/variables")({
  validateSearch: (search: Record<string, unknown>): VariablesSearch => {
    const out: VariablesSearch = {};
    if (typeof search["environment"] === "string" && search["environment"] !== "")
      out.environment = search["environment"];
    if (typeof search["q"] === "string" && search["q"] !== "") out.q = search["q"];
    if (typeof search["app"] === "string" && search["app"] !== "") out.app = search["app"];
    return out;
  },
  component: function Variables() {
    const { environment, q, app } = Route.useSearch();
    const navigate = useNavigate();
    return (
      <VariablesPage
        environment={environment}
        query={q}
        app={app}
        onEnvironment={(id, query, only) =>
          void navigate({
            to: "/variables",
            search: {
              ...(id ? { environment: id } : {}),
              ...(query ? { q: query } : {}),
              ...(only ? { app: only } : {}),
            },
            replace: true,
          })
        }
      />
    );
  },
});
