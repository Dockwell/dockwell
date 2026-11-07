// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { IntegrationsPage } from "../../../features/settings/IntegrationsPage";

/** What Dockwell connects to: storage backups go to, and what routes sit behind. */
export const Route = createFileRoute("/_shell/_settings/settings/integrations")({
  component: IntegrationsPage,
});
