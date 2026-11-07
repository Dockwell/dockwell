// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { SettingsLayout } from "../../features/settings/SettingsLayout";

/** Settings' sections beside the one that's open, your profile among them, at their own paths. */
export const Route = createFileRoute("/_shell/_settings")({
  component: SettingsLayout,
});
