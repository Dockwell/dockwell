// SPDX-License-Identifier: AGPL-3.0-only

import type { Server, ServerStats } from "@dockwell/api";
import { cx, Metric, type MetricProps } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { formatBitRate, formatBytes, formatFixed, formatLiveBytesOf, formatPercent } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { trend } from "./history";
import { HIGH_USE } from "./server";
import "./ServerMetrics.css";

export interface ServerMetricsProps {
  server: Server;
  /** The fourth number: network on the overview, load on the server's own page. */
  fourth: "network" | "load";
  /** Phones: the numbers alone, two by two, without trends or captions. */
  compact?: boolean;
  /** Nothing can say how the server is now: what it last said is greyed, not shown as now. */
  stale?: boolean;
}

/**
 * A server's live CPU, memory, disk and network or load, each with its
 * trend over the history the control plane keeps, since the last gap. Values keep their number
 * of decimals, so they update in place without shifting.
 */
export function ServerMetrics({ server, fourth, compact = false, stale = false }: ServerMetricsProps) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const of = (total: string) => t`of ${total}`;
  const stats = server.stats;
  if (!stats) return null;
  const locale = i18n.locale;
  // Trends start after the last gap in reporting, so an outage isn't drawn as part of one.
  const { samples: history, spanMs } = trend(server.history.length > 0 ? server.history : [stats]);
  const cpus = server.inventory?.cpuCount ?? 0;
  const span = words.span(spanMs);
  const cpuAverage = formatPercent(
    history.reduce((sum, s) => sum + s.cpuPercent, 0) / Math.max(history.length, 1),
    locale,
  );

  const [memUsed, memTotal] = formatLiveBytesOf(stats.memoryUsedBytes, stats.memoryTotalBytes, locale);
  const memShare = formatPercent(share(stats.memoryUsedBytes, stats.memoryTotalBytes), locale);
  const disk = diskTotals(stats);
  const [diskUsed, diskTotal] = formatLiveBytesOf(disk.used, disk.size, locale);
  const free = formatBytes(disk.size - disk.used, locale);
  const [rate, rateUnit] = formatBitRate(stats.networkRxBytesPerSecond + stats.networkTxBytesPerSecond, locale);
  const rx = formatBitRate(stats.networkRxBytesPerSecond, locale).join(" ");
  const tx = formatBitRate(stats.networkTxBytesPerSecond, locale).join(" ");
  const loads = history.map((s) => s.load1);

  const metrics: (MetricProps & { id: string })[] = [
    {
      id: "cpu",
      label: t`CPU`,
      value: formatFixed(stats.cpuPercent, 1, locale),
      unit: "%",
      data: history.map((s) => s.cpuPercent),
      min: 0,
      max: 100,
      threshold: HIGH_USE,
      ...(stats.cpuPercent > HIGH_USE ? { tone: "warning" as const } : {}),
      caption:
        spanMs > 0 ? t`${cpuAverage} on average over the last ${span}` : cpus > 0 ? t`Across ${cpus} vCPU` : undefined,
    },
    {
      id: "memory",
      label: t`Memory`,
      value: memUsed,
      unit: of(memTotal),
      data: history.map((s) => share(s.memoryUsedBytes, s.memoryTotalBytes)),
      min: 0,
      max: 100,
      threshold: HIGH_USE,
      ...(share(stats.memoryUsedBytes, stats.memoryTotalBytes) > HIGH_USE ? { tone: "warning" as const } : {}),
      caption: t`${memShare} in use`,
    },
    disk.size > 0
      ? {
          id: "disk",
          label: t`Disk`,
          value: diskUsed,
          unit: of(diskTotal),
          data: history.map((s) => {
            const totals = diskTotals(s);
            return share(totals.used, totals.size);
          }),
          min: 0,
          max: 100,
          caption: t`${free} free`,
        }
      : { id: "disk", label: t`Disk`, value: "–", caption: t`The agent reports no disks` },
    fourth === "network"
      ? {
          id: "network",
          label: t`Network`,
          value: rate,
          unit: rateUnit,
          tone: "cobalt",
          data: history.map((s) => Number(s.networkRxBytesPerSecond + s.networkTxBytesPerSecond)),
          min: 0,
          caption: t`${rx} in · ${tx} out`,
        }
      : {
          id: "load",
          label: t`Load average`,
          value: formatFixed(stats.load1, 2, locale),
          unit: `/ ${formatFixed(stats.load5, 2, locale)} / ${formatFixed(stats.load15, 2, locale)}`,
          data: loads,
          min: 0,
          max: Math.max(cpus, ...loads),
          caption: cpus > 0 ? t`1, 5 and 15 min, on ${cpus} vCPU` : t`1, 5 and 15 min`,
        },
  ];

  return (
    <div className={cx("metrics", compact && "is-compact", stale && "is-stale")} aria-disabled={stale || undefined}>
      {metrics.map(({ id, data, caption, ...metric }) => (
        <div key={id} className="dw-panel metrics-panel">
          <Metric {...metric} data={compact ? undefined : data} caption={compact ? undefined : caption} />
        </div>
      ))}
    </div>
  );
}

/** A part of a whole as a percentage, 0 when the whole is unknown. */
function share(part: number | bigint, whole: number | bigint): number {
  const w = Number(whole);
  return w > 0 ? (Number(part) / w) * 100 : 0;
}

/** Every disk the agent reports, added up. */
function diskTotals(stats: ServerStats): { used: number; size: number } {
  let used = 0;
  let size = 0;
  for (const disk of stats.disks) {
    used += Number(disk.usedBytes);
    size += Number(disk.sizeBytes);
  }
  return { used, size };
}
