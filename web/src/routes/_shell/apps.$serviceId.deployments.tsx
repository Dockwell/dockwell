// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AppDeployments } from "../../features/apps/AppDeployments";

export const Route = createFileRoute("/_shell/apps/$serviceId/deployments")({
  validateSearch: (search: Record<string, unknown>): { revision?: string } => {
    const revision = search["revision"];
    // A number in the address reads back as a number.
    return typeof revision === "string" || typeof revision === "number" ? { revision: String(revision) } : {};
  },
  component: AppDeployments,
});
