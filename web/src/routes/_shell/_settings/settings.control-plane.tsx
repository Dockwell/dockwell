// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ControlPlanePage } from "../../../features/settings/ControlPlanePage";

/** What runs the panel and API, how it's doing, and how to back it up and upgrade it. */
export const Route = createFileRoute("/_shell/_settings/settings/control-plane")({
  component: ControlPlanePage,
});
