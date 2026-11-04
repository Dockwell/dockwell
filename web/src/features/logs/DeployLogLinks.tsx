// SPDX-License-Identifier: AGPL-3.0-only

import type { Change } from "@dockwell/api";
import { Icon } from "@dockwell/ui";
import { Trans } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import "./DeployLogLinks.css";

/**
 * Links to the logs of what a failed change deployed, which is where to look
 * next: a server keeps them after the rollback removes the container. A change
 * that touched no service has none to link to, and a plan in brief, which has no
 * resources, shows none until the whole of it is read.
 */
export function FailedDeployLogs({ change }: { change: Change }) {
  const services = (change.plan?.resources ?? []).filter((r) => r.resource?.kind === "service");
  if (services.length === 0) return null;
  return (
    <div className="deploy-logs">
      {services.map((s) => (
        <Link
          key={s.id}
          className="deploy-logs-link"
          to="/apps/$serviceId/logs"
          params={{ serviceId: s.id }}
          search={{ change: change.id }}
        >
          <Icon name="terminal" size={14} />
          {services.length > 1 ? (
            <Trans>Read {s.resource?.name}'s logs from this deploy</Trans>
          ) : (
            <Trans>Read this deploy's logs</Trans>
          )}
        </Link>
      ))}
    </div>
  );
}
