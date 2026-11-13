// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AppEnvironment } from "../../features/apps/AppEnvironment";

interface EnvironmentSearch {
  /** A value to open for a change, by name, as Variables and secrets links to one. */
  change?: string;
}

export const Route = createFileRoute("/_shell/apps/$serviceId/environment")({
  validateSearch: (search: Record<string, unknown>): EnvironmentSearch =>
    typeof search["change"] === "string" && search["change"] !== "" ? { change: search["change"] } : {},
  component: function Environment() {
    const { change } = Route.useSearch();
    return <AppEnvironment key={change ?? ""} changeFirst={change} />;
  },
});
