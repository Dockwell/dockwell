// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { SecurityPage } from "../../../features/settings/SecurityPage";

/** The least anyone may choose for how often their passkey is asked for again. */
export const Route = createFileRoute("/_shell/_settings/settings/security")({
  component: SecurityPage,
});
