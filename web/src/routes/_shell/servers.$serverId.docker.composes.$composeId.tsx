// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { readComposeDetailSearch } from "../../features/docker/search";

export const Route = createFileRoute("/_shell/servers/$serverId/docker/composes/$composeId")({
  validateSearch: readComposeDetailSearch,
});
