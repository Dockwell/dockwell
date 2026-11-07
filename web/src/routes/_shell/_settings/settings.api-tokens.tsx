// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { TokensPage } from "../../../features/settings/TokensPage";

/** Your API tokens: where each was last used, revoking one, and making another. */
export const Route = createFileRoute("/_shell/_settings/settings/api-tokens")({
  component: TokensPage,
});
