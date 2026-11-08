// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { WebsiteOverview } from "../../features/websites/WebsiteOverview";

/** A website at a glance: how each part of what serves it runs, and what it serves. */
export const Route = createFileRoute("/_shell/websites/$websiteId/")({
  component: WebsiteOverview,
});
