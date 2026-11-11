// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AcceptInvitePage } from "../features/auth/AcceptInvitePage";

export const Route = createFileRoute("/invite")({
  staticData: { public: true },
  component: AcceptInvitePage,
});
