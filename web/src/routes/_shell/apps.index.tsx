// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AppsPage } from "../../features/apps/AppsPage";

export const Route = createFileRoute("/_shell/apps/")({
  component: AppsPage,
});
