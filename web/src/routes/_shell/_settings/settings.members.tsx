// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { MembersPage } from "../../../features/settings/MembersPage";

/** Who signs in to the install, their role and how they sign in. */
export const Route = createFileRoute("/_shell/_settings/settings/members")({
  component: MembersPage,
});
