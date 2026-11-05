// SPDX-License-Identifier: AGPL-3.0-only

import { Trans, useLingui } from "@lingui/react/macro";
import { createFileRoute } from "@tanstack/react-router";

import { useApp } from "../../features/apps/AppContext";
import { containerName } from "../../features/apps/app";
import { CopyButton } from "../../features/servers/CopyButton";
import { EmptyState } from "../../features/shell/EmptyState";

/** A place for the logs tab to go. The logs task (#30) replaces it with the stream. */
export const Route = createFileRoute("/_shell/apps/$serviceId/logs")({
  component: function Logs() {
    const { t } = useLingui();
    const { service } = useApp();
    const command = `docker logs ${containerName(service)}`;
    return (
      <EmptyState icon="terminal" title={<Trans>Logs aren't in this build yet</Trans>}>
        <p>
          <Trans>Live logs arrive in this tab with a later change. Until then, read them on the server:</Trans>
        </p>
        <p className="app-logs-command">
          <code>{command}</code>
          <CopyButton text={command} label={t`Copy the command`} />
        </p>
      </EmptyState>
    );
  },
});
