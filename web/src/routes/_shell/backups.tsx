// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { BackupsPage } from "../../features/backups/BackupsPage";

/** Every backup, with the way to take one and to restore one. */
export const Route = createFileRoute("/_shell/backups")({ component: BackupsPage });
