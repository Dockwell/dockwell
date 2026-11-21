// SPDX-License-Identifier: AGPL-3.0-only

import { createRouter } from "@tanstack/react-router";

import { RoutePending } from "../features/shell/RoutePending";
import { routeTree } from "../routeTree.gen";

export const router = createRouter({
  routeTree,
  defaultPreload: "intent",
  scrollRestoration: true,
  // A screen whose code is still on its way shows a page's shape, not nothing; the
  // shapes wait 150 ms more themselves, so a fast load still shows neither.
  defaultPendingComponent: RoutePending,
  defaultPendingMs: 150,
  defaultPendingMinMs: 0,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
  interface StaticDataRouteOption {
    /** The page works without signing in: the claim and sign-in screens. */
    public?: boolean;
  }
}
