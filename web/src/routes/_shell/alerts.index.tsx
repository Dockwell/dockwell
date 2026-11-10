// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AlertsTab } from "../../features/alerts/ListTabs";

/** What's firing now, and what fired before. */
export const Route = createFileRoute("/_shell/alerts/")({ component: AlertsTab });
