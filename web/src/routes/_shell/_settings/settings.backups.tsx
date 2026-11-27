// SPDX-License-Identifier: AGPL-3.0-only

import { ControlBackupService } from "@dockwell/api";
import { Trans, useLingui } from "@lingui/react/macro";
import { createFileRoute } from "@tanstack/react-router";
import { useMay } from "../../../features/auth/may";
import { ControlBackupPanel } from "../../../features/backups/ControlBackup";
import { DestinationsPanel, useDestinations } from "../../../features/backups/DestinationsPanel";
import { Page } from "../../../features/shell/Page";
import { RoleRefused } from "../../../features/shell/ReadFailed";
import { RouterButtonLink } from "../../../lib/links";

export const Route = createFileRoute("/_shell/_settings/settings/backups")({ component: SettingsBackups });

function SettingsBackups() {
  const { t } = useLingui();
  const may = useMay()(ControlBackupService.method.getControlBackup);
  const destinations = useDestinations();
  return (
    <Page
      title={t`Backups`}
      width="narrow"
      back="/settings"
      actions={
        <RouterButtonLink to="/backups">
          <Trans>App backups</Trans>
        </RouterButtonLink>
      }
    >
      {may.allowed ? <ControlBackupPanel /> : may.why === "pending" ? null : <RoleRefused />}
      <DestinationsPanel
        destinations={destinations.data?.destinations ?? []}
        pending={destinations.isPending}
        offerSFTP
      />
    </Page>
  );
}
