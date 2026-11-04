// SPDX-License-Identifier: AGPL-3.0-only

import { createRootRoute, Outlet } from "@tanstack/react-router";

import { AuthGate } from "../features/auth/AuthGate";
import { NotFound } from "../features/system/NotFound";

export const Route = createRootRoute({
  component: () => (
    <AuthGate>
      <Outlet />
    </AuthGate>
  ),
  notFoundComponent: NotFound,
});
