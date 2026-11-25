// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { DatabaseSchemas } from "../../features/databases/DatabaseSchemas";

/** The logical databases inside a PostgreSQL, MySQL or MariaDB server. */
export const Route = createFileRoute("/_shell/databases/$serviceId/databases")({ component: DatabaseSchemas });
