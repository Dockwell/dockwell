// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AlertPage } from "../../features/alerts/AlertPage";

/** One alert: what fired, what changed around it, what was sent, and how it ended. */
export const Route = createFileRoute("/_shell/alerts/$alertId")({ component: AlertRoute });

function AlertRoute() {
  const { alertId } = Route.useParams();
  return <AlertPage alertId={alertId} />;
}
