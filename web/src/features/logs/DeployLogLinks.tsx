// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type Change, LogService } from "@dockwell/api";
import { Icon } from "@dockwell/ui";
import { Trans } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import "./DeployLogLinks.css";

/**
 * A link to the logs of what one deploy ran, if there are any to read: a deploy
 * that never made a container has none, and a server may have let go of them. A
 * server that can't be asked may still have them, so the link stands while that
 * isn't known.
 */
export function DeployLogLink({
  serviceId,
  change,
  revision,
  name,
}: {
  serviceId: string;
  change?: string | undefined;
  revision?: bigint | undefined;
  /** Says which app, when a deploy may be of several. */
  name?: string | undefined;
}) {
  const sources = useQuery(
    LogService.method.listLogSources,
    { id: serviceId },
    { retry: false, refetchOnWindowFocus: false },
  );
  const data = sources.data;
  if (!data) return null;
  const has = data.sources.some((s) => (change && s.changeId === change) || (revision && s.revision === revision));
  if (!has && data.servers.length === 0) return null;
  return (
    <Link
      className="deploy-logs-link"
      to="/apps/$serviceId/logs"
      params={{ serviceId }}
      search={change ? { change } : { revision: Number(revision ?? 0) }}
    >
      <Icon name="terminal" size={14} />
      {name ? <Trans>Read {name}'s logs from this deploy</Trans> : <Trans>Read this deploy's logs</Trans>}
    </Link>
  );
}

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
        <DeployLogLink
          key={s.id}
          serviceId={s.id}
          change={change.id}
          name={services.length > 1 ? s.resource?.name : undefined}
        />
      ))}
    </div>
  );
}
