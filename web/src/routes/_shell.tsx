// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, Outlet } from "@tanstack/react-router";

import { Shell } from "../features/shell/Shell";

export const Route = createFileRoute("/_shell")({
  component: () => (
    <Shell>
      <Outlet />
    </Shell>
  ),
});
