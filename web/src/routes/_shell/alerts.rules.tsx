// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { RulesTab } from "../../features/alerts/ListTabs";

/** What Dockwell watches for, and where each rule sends. */
export const Route = createFileRoute("/_shell/alerts/rules")({ component: RulesTab });
