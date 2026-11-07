// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, redirect } from "@tanstack/react-router";

/** A website opens on its configuration, its first tab. */
export const Route = createFileRoute("/_shell/websites/$websiteId/")({
  beforeLoad: ({ params }) => {
    throw redirect({ to: "/websites/$websiteId/configuration", params, replace: true });
  },
});
