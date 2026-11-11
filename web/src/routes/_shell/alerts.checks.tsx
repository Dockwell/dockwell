// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ChecksTab } from "../../features/alerts/ChecksTab";

/** The addresses Dockwell asks, how each stands, and its last answers. */
export const Route = createFileRoute("/_shell/alerts/checks")({ component: ChecksTab });
