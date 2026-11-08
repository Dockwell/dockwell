// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { DatabaseOverview } from "../../features/databases/DatabaseOverview";

/** A database at a glance. */
export const Route = createFileRoute("/_shell/databases/$serviceId/")({ component: DatabaseOverview });
