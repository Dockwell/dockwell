// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AppEnvironment } from "../../features/apps/AppEnvironment";

export const Route = createFileRoute("/_shell/apps/$serviceId/environment")({
  component: AppEnvironment,
});
