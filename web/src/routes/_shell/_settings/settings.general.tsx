// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { GeneralPage } from "../../../features/settings/GeneralPage";

/** The install's name, time zone, panel address and language. */
export const Route = createFileRoute("/_shell/_settings/settings/general")({
  component: GeneralPage,
});
