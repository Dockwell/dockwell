// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ConnectionPage } from "../../features/connections/ConnectionPage";

/** One connection: where its bucket is, what reads it, and the way to use it in an app (ADR 0099). */
export const Route = createFileRoute("/_shell/connections/$name")({
  component: function Connection() {
    const { name } = Route.useParams();
    return <ConnectionPage name={name} />;
  },
});
