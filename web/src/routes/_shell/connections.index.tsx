// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ConnectionsPage } from "../../features/connections/ConnectionsPage";

/** Every connection: the buckets apps keep files in, and the way to add one (ADR 0099). */
export const Route = createFileRoute("/_shell/connections/")({ component: ConnectionsPage });
