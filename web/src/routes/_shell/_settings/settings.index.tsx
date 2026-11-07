// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { SettingsHome } from "../../../features/settings/SettingsLayout";

/** Settings: its sections on a phone, and General on wide screens. */
export const Route = createFileRoute("/_shell/_settings/settings/")({
  component: SettingsHome,
});
