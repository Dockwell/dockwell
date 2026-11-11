// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { SourcesPage } from "../../features/catalog/SourcesPage";

export const Route = createFileRoute("/_shell/catalog/sources")({
  component: SourcesPage,
});
