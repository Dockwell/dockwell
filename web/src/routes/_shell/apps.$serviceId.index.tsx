// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AppOverview } from "../../features/apps/AppOverview";

export const Route = createFileRoute("/_shell/apps/$serviceId/")({
  validateSearch: (search: Record<string, unknown>): { add?: "domain" | "port" } => {
    const add = search["add"];
    return add === "domain" || add === "port" ? { add } : {};
  },
  component: AppOverview,
});
