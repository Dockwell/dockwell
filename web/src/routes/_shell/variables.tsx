// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { type Source, VariablesPage } from "../../features/variables/VariablesPage";

interface VariablesSearch {
  environment?: string;
  q?: string;
  /** An app, by ID: only what it runs with. */
  app?: string;
  /** Only what's shared, or only what's set on apps (#618). */
  from?: Source;
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
    if (search["from"] === "shared" || search["from"] === "apps") out.from = search["from"];
    return out;
  },
  component: function Variables() {
    const { environment, q, app, from } = Route.useSearch();
    const navigate = useNavigate();
    return (
      <VariablesPage
        environment={environment}
        query={q}
        app={app}
        from={from}
        onEnvironment={(id, query, only, source) =>
          void navigate({
            to: "/variables",
            search: {
              ...(id ? { environment: id } : {}),
              ...(query ? { q: query } : {}),
              ...(only ? { app: only } : {}),
              ...(source ? { from: source } : {}),
            },
            replace: true,
          })
        }
      />
    );
  },
});
