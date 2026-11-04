// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { MorePage } from "../../features/shell/MorePage";

export const Route = createFileRoute("/_shell/more")({
  component: MorePage,
});
