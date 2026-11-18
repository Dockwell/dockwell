// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ChannelsTab } from "../../features/alerts/ChannelsTab";

/** Where alerts go, with a test for each. */
export const Route = createFileRoute("/_shell/alerts/channels")({
  component: ChannelsTab,
  // A channel to open, by name, as a link from Alerts names it.
  validateSearch: (search: Record<string, unknown>): { edit?: string } =>
    typeof search["edit"] === "string" && search["edit"] !== "" ? { edit: search["edit"] } : {},
});
