// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

export const Route = createFileRoute("/_shell/apps/templates")({
  validateSearch: (search: Record<string, unknown>): { project?: string; template?: string } => ({
    ...(typeof search["project"] === "string" ? { project: search["project"] } : {}),
    ...(typeof search["template"] === "string" ? { template: search["template"] } : {}),
  }),
});
