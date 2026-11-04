// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ClaimPage } from "../features/auth/ClaimPage";

export const Route = createFileRoute("/claim")({
  staticData: { public: true },
  component: ClaimPage,
});
