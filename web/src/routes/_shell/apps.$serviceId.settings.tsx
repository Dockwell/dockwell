// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AppSettings } from "../../features/apps/AppSettings";

export const Route = createFileRoute("/_shell/apps/$serviceId/settings")({
  component: AppSettings,
});
