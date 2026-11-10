// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ChannelsTab } from "../../features/alerts/ChannelsTab";

/** Where alerts go, with a test for each. */
export const Route = createFileRoute("/_shell/alerts/channels")({ component: ChannelsTab });
