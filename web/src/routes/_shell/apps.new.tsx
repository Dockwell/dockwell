// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AppsPage } from "../../features/apps/AppsPage";

/** New app: its sheet over the list, as Apps-New draws it, at an address of its own. */
export const Route = createFileRoute("/_shell/apps/new")({
  component: AppsPage,
});
