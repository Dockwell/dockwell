// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { WebsiteFiles } from "../../features/websites/WebsiteFiles";

/** A website's files: what's at the top of its folder, each opening in Files. */
export const Route = createFileRoute("/_shell/websites/$websiteId/files")({
  component: WebsiteFiles,
});
