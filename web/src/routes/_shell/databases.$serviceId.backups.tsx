// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { DatabaseBackups } from "../../features/databases/DatabaseBackups";

/** A database's backups, each of which restores through a plan. */
export const Route = createFileRoute("/_shell/databases/$serviceId/backups")({ component: DatabaseBackups });
