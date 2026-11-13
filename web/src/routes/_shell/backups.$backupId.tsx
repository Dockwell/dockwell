// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { BackupPage } from "../../features/backups/BackupPage";

/** One backup: what it's of, where it's kept, and its restore, check and delete. */
export const Route = createFileRoute("/_shell/backups/$backupId")({
  component: function BackupRoute() {
    const { backupId } = Route.useParams();
    return <BackupPage backupId={backupId} />;
  },
});
