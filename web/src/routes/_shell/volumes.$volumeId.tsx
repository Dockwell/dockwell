// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { VolumePage } from "../../features/volumes/VolumePage";

/** One volume: what it holds, who mounts it, and how it's backed up. */
export const Route = createFileRoute("/_shell/volumes/$volumeId")({
  component: function Volume() {
    const { volumeId } = Route.useParams();
    return <VolumePage volumeId={volumeId} />;
  },
});
