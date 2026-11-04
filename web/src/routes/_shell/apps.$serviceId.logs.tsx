// SPDX-License-Identifier: AGPL-3.0-only

import { Trans } from "@lingui/react/macro";
import { createFileRoute } from "@tanstack/react-router";

import { EmptyState } from "../../features/shell/EmptyState";

/** A place for the logs tab to go. The logs task (#30) replaces it with the stream. */
export const Route = createFileRoute("/_shell/apps/$serviceId/logs")({
  component: function Logs() {
    return (
      <EmptyState icon="terminal" title={<Trans>Logs aren't in this build yet</Trans>}>
        <p>
          <Trans>
            Live logs arrive in this tab with a later change. Until then, read them with docker logs on the server.
          </Trans>
        </p>
      </EmptyState>
    );
  },
});
