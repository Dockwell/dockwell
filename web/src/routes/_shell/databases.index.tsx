// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { DatabasesPage } from "../../features/databases/DatabasesPage";

/** Every database, with how each is, the way to make another, and their backups. */
export const Route = createFileRoute("/_shell/databases/")({ component: DatabasesPage });
