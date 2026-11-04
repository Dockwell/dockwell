// SPDX-License-Identifier: AGPL-3.0-only

import { durationMs } from "@bufbuild/protobuf/wkt";
import { Connection, type Server } from "@dockwell/api";
import { Panel, StatusBadge, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { formatBytesOf, formatPercent } from "../../lib/format";
import { Connecting, Reconnecting } from "./FleetNotes";
import { serverStatus } from "./server";
import { useFleet } from "./useFleet";
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
  return (
    <Panel
      title={t`Servers`}
      actions={reconnectingNote && fleet.status === "reconnecting" ? <Reconnecting /> : null}
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
            <ServerRow key={s.id} server={s} linked={linked} />
          ))}
        </ul>
      )}
    </Panel>
  );
}

function ServerRow({ server, linked }: { server: Server; linked: boolean }) {
  const { t, i18n } = useLingui();
  const stats = server.stats;
  const [used, total] = stats ? formatBytesOf(stats.memoryUsedBytes, stats.memoryTotalBytes, i18n.locale) : ["", ""];
  const up = stats?.uptime ? splitUptime(durationMs(stats.uptime)) : undefined;
  const count = up?.count ?? 0;
  const uptime = !up ? "–" : up.unit === "d" ? t`${count} d` : up.unit === "h" ? t`${count} h` : t`${count} min`;
  const connected = server.connection === Connection.CONNECTED;
  const meta =
    server.connection === Connection.PENDING
      ? t`Waiting for its agent`
      : server.connection === Connection.UNREACHABLE
        ? t`Agent not connected`
        : undefined;
  return (
    <li className="servers-row">
      <div className="servers-name">
        <StatusBadge
          status={serverStatus(server)}
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

/** Uptime rounded to what a person acts on: days, then hours, then minutes. */
function splitUptime(ms: number): { count: number; unit: "d" | "h" | "min" } {
  const minutes = Math.floor(ms / 60_000);
  const hours = Math.floor(minutes / 60);
  const days = Math.floor(hours / 24);
  if (days >= 1) return { count: days, unit: "d" };
  if (hours >= 1) return { count: hours, unit: "h" };
  return { count: minutes, unit: "min" };
}
