// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Connection, EventKind, type Server } from "@dockwell/api";
import {
  cx,
  FleetGrid,
  type FleetServer,
  Metric,
  type MetricProps,
  Panel,
  PHONE_QUERY,
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
import { primaryAddress, serverStatus } from "../servers/server";
import { useControlPlaneNow, useFleetUnread } from "../servers/useFleet";
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
        <RecentChanges kinds={FLEET_EVENTS} className="overview-fleet-changes" />
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

/**
 * Every server as a tile, as Overview-Fleet draws them, each opening its page.
 * One group: servers have no region to group them by yet (#319).
 */
function FleetMap({ servers, compact }: { servers: readonly Server[]; compact: boolean }) {
  const { t } = useLingui();
  const navigate = useNavigate();
  const unknown = useFleetUnread() !== undefined;
  const tiles = useTiles(servers, unknown);
  const grid = (
    <FleetGrid
      servers={tiles}
      size={compact || servers.length > 200 ? "sm" : "md"}
      readout={!compact}
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
    </Panel>
  );
}

/**
 * Each server as a tile names it: its state, its numbers while it reports, its address, and why it's not well.
 * While what the fleet last said can't stand, nobody can say how any server is, so each reads Unknown.
 */
function useTiles(servers: readonly Server[], unknown: boolean): FleetServer[] {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(15_000);
  return servers.map((s) => {
    const stats = s.connection === Connection.CONNECTED ? s.stats : undefined;
    const total = Number(stats?.memoryTotalBytes ?? 0n);
    const seen = s.lastSeenTime ? words.ago(timestampDate(s.lastSeenTime), now) : undefined;
    const note =
      s.connection === Connection.UNREACHABLE
        ? seen
          ? t`Last reported ${seen}`
          : t`Agent not connected`
        : s.connection === Connection.PENDING
          ? t`Waiting for its agent`
          : s.dockerUnreadableSince
            ? t`Can't read Docker`
            : undefined;
    if (unknown) return { id: s.id, name: s.name, status: "unknown", ip: primaryAddress(s.inventory?.addresses ?? []) };
    return {
      id: s.id,
      name: s.name,
      status: serverStatus(s),
      cpu: stats ? Math.round(stats.cpuPercent) : null,
      mem: stats && total > 0 ? Math.round((Number(stats.memoryUsedBytes) / total) * 100) : null,
      ip: primaryAddress(s.inventory?.addresses ?? []),
      note,
    };
  });
}
