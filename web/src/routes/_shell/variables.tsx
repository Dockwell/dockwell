// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { VariablesPage } from "../../features/variables/VariablesPage";

interface VariablesSearch {
  environment?: string;
}

/**
 * An environment's variables and secrets. The command bar, More on phones
 * and Activity reach it; the app screens (#31) will show an app's share of
 * the same list.
 */
export const Route = createFileRoute("/_shell/variables")({
  validateSearch: (search: Record<string, unknown>): VariablesSearch =>
    typeof search["environment"] === "string" && search["environment"] !== ""
      ? { environment: search["environment"] }
      : {},
  component: function Variables() {
    const { environment } = Route.useSearch();
    const navigate = useNavigate();
    return (
      <VariablesPage
        environment={environment}
        onEnvironment={(id) => void navigate({ to: "/variables", search: { environment: id }, replace: true })}
      />
    );
  },
});
