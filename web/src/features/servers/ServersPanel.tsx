// SPDX-License-Identifier: AGPL-3.0-only

import { Connection, type Server, ServerStrain } from "@dockwell/api";
import { Panel, StatusBadge, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { formatBytesOf, formatPercent } from "../../lib/format";
import { Connecting, FleetUnknown, Reconnecting } from "./FleetNotes";
import { HIGH_USE, serverStatus, useServerWords } from "./server";
import { useFleet, useFleetUnread } from "./useFleet";
import "./ServersPanel.css";

export interface ServersPanelProps {
  /** Each name opens the server's page; that needs the router, which the status page doesn't have. */
  linked?: boolean;
  /**
   * Says "Reconnecting" in the panel's header. Pages in the shell say it in
   * their own head instead, so it shows and is announced once.
   */
  reconnectingNote?: boolean;
}

/** Every server with its live stats, as their agents report them. */
export function ServersPanel({ linked = false, reconnectingNote = true }: ServersPanelProps) {
  const { t } = useLingui();
  const fleet = useFleet();
  const unread = useFleetUnread();
  return (
    <Panel
      title={t`Servers`}
      actions={
        unread ? (
          <FleetUnknown why={unread.why} heard={unread.heard} />
        ) : reconnectingNote && fleet.status === "reconnecting" ? (
          <Reconnecting />
        ) : null
      }
      flush
    >
      {fleet.status === "connecting" ? (
        <div className="servers-empty">
          <Connecting error={fleet.error} />
        </div>
      ) : fleet.servers.length === 0 ? (
        <p className="servers-empty">
          <Trans>
            No servers yet. Add one with <code>dockwell token create</code> on this server.
          </Trans>
        </p>
      ) : (
        <ul className="servers-list">
          {fleet.servers.map((s) => (
            <ServerRow key={s.id} server={s} linked={linked} unknown={unread !== undefined} />
          ))}
        </ul>
      )}
    </Panel>
  );
}

function ServerRow({ server, linked, unknown }: { server: Server; linked: boolean; unknown: boolean }) {
  const { t, i18n } = useLingui();
  const stats = server.stats;
  const [used, total] = stats ? formatBytesOf(stats.memoryUsedBytes, stats.memoryTotalBytes, i18n.locale) : ["", ""];
  const { uptime = "–" } = useServerWords(server);
  const connected = server.connection === Connection.CONNECTED;
  const meta =
    server.connection === Connection.PENDING
      ? t`Waiting for its agent`
      : server.connection === Connection.UNREACHABLE
        ? t`Agent not connected`
        : server.dockerUnreadableSince
          ? t`Can't read Docker`
          : server.strainedSince
            ? server.strain === ServerStrain.CPU
              ? t`CPU above ${HIGH_USE}%`
              : t`Memory above ${HIGH_USE}%`
            : undefined;
  return (
    <li className="servers-row">
      <div className="servers-name">
        <StatusBadge
          status={unknown ? "unknown" : serverStatus(server)}
          label={
            linked ? (
              <Link to="/servers/$serverId" params={{ serverId: server.id }} className="servers-link">
                {server.name}
              </Link>
            ) : (
              server.name
            )
          }
          meta={meta}
        />
        {server.controlServer ? <Tag className="servers-role">{t`Control server`}</Tag> : null}
      </div>
      <p className="servers-os">{server.inventory?.osName || t`Unknown system`}</p>
      {connected && stats ? (
        <dl className="servers-stats">
          <div>
            <dt>
              <Trans>CPU</Trans>
            </dt>
            <dd>{formatPercent(stats.cpuPercent, i18n.locale)}</dd>
          </div>
          <div>
            <dt>
              <Trans>Memory</Trans>
            </dt>
            <dd>
              <Trans>
                {used} of {total}
              </Trans>
            </dd>
          </div>
          <div>
            <dt>
              <Trans>Up</Trans>
            </dt>
            <dd>{uptime}</dd>
          </div>
        </dl>
      ) : null}
    </li>
  );
}
