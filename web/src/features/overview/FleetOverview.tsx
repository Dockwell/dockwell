// SPDX-License-Identifier: AGPL-3.0-only

import { EventKind, type Server } from "@dockwell/api";
import {
  cx,
  FleetGrid,
  type FleetServer,
  Metric,
  type MetricProps,
  Panel,
  PHONE_QUERY,
  type Status,
  StatusBadge,
  useMediaQuery,
  useUIStrings,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { formatFixed, formatLiveBytesOf } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { FleetUnreadNote } from "../servers/FleetNotes";
import { byNeed, NEED } from "../servers/server";
import { useServerTiles } from "../servers/tiles";
import { useFleetUnread } from "../servers/useFleet";
import { Page } from "../shell/Page";
import { shortVersion } from "../shell/version";
import { FleetCallout } from "./FleetCallout";
import { agentVersions, fleetCounts, fleetNumbers } from "./fleet";
import { OverviewApps } from "./OverviewApps";
import { RecentChanges } from "./RecentChanges";
import "../servers/ServerMetrics.css";
import "./Overview.css";

/**
 * What the fleet's overview shows as recent: changes, and apps' health as it moves.
 * Alerts, once #304 lands, belong here too.
 */
const FLEET_EVENTS: readonly EventKind[] = [EventKind.CHANGE, EventKind.HEALTH];

/**
 * The overview from the second server on, as Overview-Fleet lays it out: what
 * needs a look first, the fleet's numbers, every server at a glance beside what
 * changed, then the apps and where each runs. On a phone, M-Overview-Fleet's
 * order, with the numbers two by two.
 */
export function FleetOverview({ servers, status }: { servers: readonly Server[]; status: ReactNode }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const phone = useMediaQuery(PHONE_QUERY);
  const unread = useFleetUnread();
  const count = strings.fleetGrid.servers(servers.length);
  // As the rail says the control plane's: a build on the way to a release is that release, dev.
  const versions = [...new Set(agentVersions(servers).map(shortVersion))];
  const [version] = versions;
  const agents =
    versions.length === 1 && version
      ? t`agent ${version} everywhere`
      : versions.length > 1
        ? plural(versions.length, { one: "agents on # version", other: "agents on # versions" })
        : "";
  return (
    // A dashboard, as one server's is: wide (Layout in design/system/README.md).
    <Page
      width="wide"
      title={t`Overview`}
      lead={[count, agents].filter(Boolean).join(" · ")}
      phoneTitle={t`Fleet`}
      phoneSub={count}
      status={status}
    >
      {unread ? <FleetUnreadNote why={unread.why} heard={unread.heard} /> : <FleetCallout servers={servers} />}
      <FleetMetrics servers={servers} compact={phone} />
      <div className="overview-fleet-row">
        <FleetMap servers={servers} compact={phone} />
        <RecentChanges kinds={FLEET_EVENTS} className="overview-fleet-changes" unboxed={phone} />
      </div>
      <OverviewApps fleet />
    </Page>
  );
}

/**
 * How many servers serve, and the fleet's CPU and memory, live, each with its
 * trend over the minutes the control plane keeps. Requests and errors wait for
 * the ingress to count them (#317).
 */
function FleetMetrics({ servers, compact }: { servers: readonly Server[]; compact: boolean }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const locale = i18n.locale;
  const counts = fleetCounts(servers);
  const numbers = fleetNumbers(servers);
  const of = (total: string) => t`of ${total}`;
  const usedOf = (used: string, total: string) => t`${used} of ${total}`;
  const why = [
    counts.down > 0 ? plural(counts.down, { one: "# down", other: "# down" }) : "",
    counts.waiting > 0 ? plural(counts.waiting, { one: "# waiting", other: "# waiting" }) : "",
    counts.degraded > 0 ? plural(counts.degraded, { one: "# can't read Docker", other: "# can't read Docker" }) : "",
  ].filter(Boolean);

  const metrics: (MetricProps & { id: string })[] = [];
  if (!compact) {
    metrics.push({
      id: "serving",
      label: t`Servers serving`,
      value: String(counts.serving),
      unit: of(String(counts.total)),
      caption: why.length > 0 ? why.join(" · ") : t`Every server is reporting`,
    });
  }
  if (numbers) {
    const { now, trend, spanMs, cpuCount: cpus } = numbers;
    const span = words.span(spanMs);
    const share = (part: number, whole: number) => (whole > 0 ? (part / whole) * 100 : 0);
    const [used, all] = formatLiveBytesOf(now.memoryUsedBytes, now.memoryTotalBytes, locale);
    metrics.push(
      {
        id: "cpu",
        label: t`CPU`,
        value: formatFixed(now.cpuPercent, 1, locale),
        unit: "%",
        data: trend.map((p) => p.cpuPercent),
        min: 0,
        max: 100,
        caption: [cpus > 0 ? t`Across ${cpus} vCPU` : "", spanMs > 0 ? t`last ${span}` : ""]
          .filter(Boolean)
          .join(" · "),
      },
      compact
        ? { id: "memory", label: t`Memory`, value: used, unit: of(all) }
        : {
            id: "memory",
            label: t`Memory`,
            value: formatFixed(share(now.memoryUsedBytes, now.memoryTotalBytes), 0, locale),
            unit: "%",
            data: trend.map((p) => share(p.memoryUsedBytes, p.memoryTotalBytes)),
            min: 0,
            max: 100,
            caption: usedOf(used, all),
          },
    );
  } else {
    const none = t`No server is reporting`;
    metrics.push(
      { id: "cpu", label: t`CPU`, value: "–", caption: none },
      { id: "memory", label: t`Memory`, value: "–", caption: none },
    );
  }

  return (
    <div className={cx("metrics", compact && "is-compact")}>
      {metrics.map(({ id, data, caption, ...metric }) => (
        <div key={id} className="dw-panel metrics-panel">
          <Metric {...metric} data={compact ? undefined : data} caption={compact ? undefined : caption} />
        </div>
      ))}
    </div>
  );
}

/** Up to this many servers, the panel names each on a line of its own under the tiles. */
const FEW_SERVERS = 6;

/** A tile's state, which says Unknown for every server once what the fleet last said can't stand. */
const tileStatus = (tile: FleetServer) => tile.status;

/**
 * Every server as a tile, as Overview-Fleet draws them, each opening its page.
 * One group: servers have no region to group them by yet (#319).
 */
function FleetMap({ servers, compact }: { servers: readonly Server[]; compact: boolean }) {
  const { t } = useLingui();
  const navigate = useNavigate();
  const unknown = useFleetUnread() !== undefined;
  const tiles = useServerTiles(servers, unknown);
  // A few servers leave the tiles room to spare beside the changes: each gets the line the
  // readout would give one, so the panel says how every server is without a hover.
  const few = !compact && tiles.length <= FEW_SERVERS;
  // Otherwise the server to look at first, named under the tiles until another is hovered,
  // as Overview-Fleet draws it, by the same ranking as the lines: any that isn't running.
  const readout = !compact && !few;
  const first = readout ? byNeed(tiles, tileStatus).find((s) => NEED[s.status] < NEED.running) : undefined;
  const grid = (
    <FleetGrid
      servers={tiles}
      size={compact || servers.length > 200 ? "sm" : "md"}
      readout={readout}
      selectedId={first?.id}
      onSelect={(server) => void navigate({ to: "/servers/$serverId", params: { serverId: server.id } })}
    />
  );
  return (
    <Panel
      title={compact ? undefined : t`Servers`}
      className="overview-fleet-map"
      actions={
        compact ? undefined : (
          <Link to="/servers" className="overview-link">
            <Trans>View all</Trans>
          </Link>
        )
      }
    >
      {grid}
      {few ? <FewServers tiles={tiles} /> : null}
    </Panel>
  );
}

/** A few servers, a line each, worst first, each opening its page. */
function FewServers({ tiles }: { tiles: readonly FleetServer[] }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const ordered = byNeed(tiles, tileStatus);
  return (
    <ul className="overview-fleet-few" aria-label={t`Each server`}>
      {ordered.map((s) => (
        <li key={s.id}>
          <Link to="/servers/$serverId" params={{ serverId: s.id }} className="overview-fleet-few-row">
            <StatusBadge status={s.status} label={s.name} />
            <span aria-hidden="true">{strings.status[s.status]}</span>
            {s.cpu != null ? (
              <span>
                {strings.fleetGrid.cpu} <b>{s.cpu}%</b>
              </span>
            ) : null}
            {s.mem != null ? (
              <span>
                {strings.fleetGrid.memory} <b>{s.mem}%</b>
              </span>
            ) : null}
            {s.ip ? <code>{s.ip}</code> : null}
            {s.note ? <span>{s.note}</span> : null}
          </Link>
        </li>
      ))}
    </ul>
  );
}
