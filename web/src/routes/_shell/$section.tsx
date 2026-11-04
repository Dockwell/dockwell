// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, notFound } from "@tanstack/react-router";

import { NotYet } from "../../features/shell/NotYet";
import { isUpcoming } from "../../features/shell/sections";

/** The design's sections that have no screen yet. A section with a screen gets its own route, which wins. */
export const Route = createFileRoute("/_shell/$section")({
  beforeLoad: ({ params }) => {
    if (!isUpcoming(params.section)) throw notFound();
  },
  component: function Section() {
    const { section } = Route.useParams();
    return isUpcoming(section) ? <NotYet section={section} /> : null;
  },
});
