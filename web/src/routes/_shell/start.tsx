// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { StartPage } from "../../features/overview/StartPage";

/** Where a newcomer starts: what their first app comes from. A claim lands here. */
export const Route = createFileRoute("/_shell/start")({
  component: StartPage,
});
