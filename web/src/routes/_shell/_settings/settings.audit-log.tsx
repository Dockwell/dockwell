// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { AuditLogPage } from "../../../features/settings/AuditLogPage";

/** Every event Dockwell recorded, narrowed and saved as a file. */
export const Route = createFileRoute("/_shell/_settings/settings/audit-log")({
  component: AuditLogPage,
});
