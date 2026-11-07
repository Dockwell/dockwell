// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { DatabaseUsers } from "../../features/databases/DatabaseUsers";

/** Who can sign in to a database, and the databases in it. */
export const Route = createFileRoute("/_shell/databases/$serviceId/users")({ component: DatabaseUsers });
