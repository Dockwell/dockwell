// SPDX-License-Identifier: AGPL-3.0-only

import { createRootRoute, Outlet } from "@tanstack/react-router";

import { AuthGate } from "../features/auth/AuthGate";
import { NotFound } from "../features/system/NotFound";
import { useDisplayTimeZone } from "../lib/format";

function Root() {
  useDisplayTimeZone();
  return (
    <AuthGate>
      <Outlet />
    </AuthGate>
  );
}

export const Route = createRootRoute({
  component: Root,
  notFoundComponent: NotFound,
});
