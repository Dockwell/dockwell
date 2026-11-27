// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { HistoryPointState, HistoryRange, type Server, type ServerHistoryPoint, ServerService } from "@dockwell/api";
import {
  Loading,
  Panel,
  SegmentedControl,
  SelectField,
  SkeletonCard,
  TimeSeriesChart,
  type TimeSeriesGapKind,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";

import { formatBitRate, formatBytes, formatClock, formatDateTime, formatDay, formatFixed } from "../../lib/format";
import { useMay } from "../auth/may";
import { ReadFailed } from "../shell/ReadFailed";
import { HIGH_USE } from "./server";
import "./ServerHistory.css";

export type RangeKey = "1h" | "6h" | "24h" | "7d" | "30d";

export const RANGE_TO_PROTO: Record<RangeKey, HistoryRange> = {
  "1h": HistoryRange.HISTORY_RANGE_1H,
  "6h": HistoryRange.HISTORY_RANGE_6H,
  "24h": HistoryRange.HISTORY_RANGE_24H,
  "7d": HistoryRange.HISTORY_RANGE_7D,
  "30d": HistoryRange.HISTORY_RANGE_30D,
};

export function gapKindOf(state: HistoryPointState): TimeSeriesGapKind | null {
  switch (state) {
    case HistoryPointState.COVERED:
      return null;
    case HistoryPointState.BEFORE_JOIN:
      return "before-join";
    case HistoryPointState.NO_CONTROL_PLANE:
      return "no-control-plane";
    case HistoryPointState.NOT_RUNNING:
      return "not-running";
    case HistoryPointState.BEFORE_CREATE:
      return "before-create";
    case HistoryPointState.NOT_TRACKED:
      return "not-tracked";
    default:
      return "offline";
  }
}

export function useHistoryFormatters(range: RangeKey) {
  const { t, i18n } = useLingui();
  const locale = i18n.locale;
  const longRange = range === "7d" || range === "30d";

  const formatTick = (ms: number) => {
    const d = new Date(ms);
    return longRange ? formatDay(d, locale) : formatClock(d, locale);
  };
  const formatHoverTime = (ms: number) => formatDateTime(new Date(ms), locale);
  const gapLabel = (kind: TimeSeriesGapKind) => {
    switch (kind) {
      case "before-join":
        return t`No data before Dockwell joined`;
      case "offline":
        return t`The agent was offline`;
      case "no-control-plane":
        return t`The control plane was offline`;
      case "not-running":
        return t`The container wasn't running`;
      case "before-create":
        return t`The container wasn't created yet`;
      case "not-tracked":
        return t`Not tracked (past the 128-container limit)`;
    }
  };
  const keyboardHint = t`Use the left and right arrows to read values over time.`;
  return { locale, formatTick, formatHoverTime, gapLabel, keyboardHint };
}

export function emptyReason(
  points: readonly { state: HistoryPointState }[],
  gapLabel: (k: TimeSeriesGapKind) => string,
  fallback: string,
): string | undefined {
  if (points.length === 0) return fallback;
  if (points.some((p) => p.state === HistoryPointState.COVERED)) return undefined;
  if (points.every((p) => p.state === HistoryPointState.BEFORE_JOIN)) return gapLabel("before-join");
  if (points.every((p) => p.state === HistoryPointState.BEFORE_CREATE)) return gapLabel("before-create");
  if (points.some((p) => p.state === HistoryPointState.NOT_TRACKED)) return gapLabel("not-tracked");
  if (points.some((p) => p.state === HistoryPointState.NOT_RUNNING)) return gapLabel("not-running");
  if (points.some((p) => p.state === HistoryPointState.OFFLINE)) return gapLabel("offline");
  if (points.some((p) => p.state === HistoryPointState.NO_CONTROL_PLANE)) return gapLabel("no-control-plane");
  return fallback;
}

function formatByteRate(bytesPerSec: number, locale: string): string {
  return `${formatBytes(Math.max(0, Math.round(bytesPerSec)), locale)}/s`;
}

function formatIops(ops: number, locale: string): string {
  if (ops >= 1000) return `${formatFixed(ops / 1000, 1, locale)}k IOPS`;
  if (ops >= 10) return `${Math.round(ops)} IOPS`;
  return `${formatFixed(ops, 1, locale)} IOPS`;
}

/**
 * Historical resource charts for one server: CPU, memory, load, disk I/O (bytes
 * and IOPS, per disk) and network (in/out, per interface) across 1 h to 30 d,
 * with synchronized hover across all five charts.
 */
export function ServerHistory({ server }: { server: Server }) {
  const { t } = useLingui();
  const may = useMay();
  const allowed = may(ServerService.method.getServerHistory).allowed;
  const [range, setRange] = useState<RangeKey>("24h");
  const [disk, setDisk] = useState("");
  const [iface, setIface] = useState("");
  const [diskMode, setDiskMode] = useState<"bytes" | "iops">("bytes");
  const [activeIndex, setActiveIndex] = useState<number | null>(null);

  const query = useQuery(
    ServerService.method.getServerHistory,
    {
      id: server.id,
      range: RANGE_TO_PROTO[range],
      disk,
      networkInterface: iface,
    },
    { enabled: allowed, retry: false, refetchInterval: 60_000, refetchOnWindowFocus: false },
  );

  const { locale, formatTick, formatHoverTime, gapLabel, keyboardHint } = useHistoryFormatters(range);

  const points: readonly ServerHistoryPoint[] = query.data?.points ?? [];
  const timestamps = useMemo(() => points.map((p) => (p.time ? timestampMs(p.time) : 0)), [points]);
  const gaps = useMemo(() => points.map((p) => gapKindOf(p.state)), [points]);
  const emptyMsg = useMemo(
    () => emptyReason(points, gapLabel, t`No history recorded for this range yet.`),
    [points, gapLabel, t],
  );

  if (!allowed) return null;

  const cpus = server.inventory?.cpuCount ?? 0;
  const memTotal = Number(server.inventory?.memoryBytes ?? server.stats?.memoryTotalBytes ?? 0n);
  const disks = query.data?.disks ?? [];
  const interfaces = query.data?.interfaces ?? [];

  const covered = (p: ServerHistoryPoint) => p.state === HistoryPointState.COVERED;

  const cpuValues = points.map((p) => (covered(p) ? p.cpuPercent : null));
  const cpuPeaks = points.map((p) => (covered(p) ? p.cpuPeakPercent : null));

  const memValues = points.map((p) => (covered(p) ? Number(p.memoryUsedBytes) : null));
  const memPeaks = points.map((p) => (covered(p) ? Number(p.memoryPeakBytes) : null));
  const maxMemTotal = points.reduce((acc, p) => Math.max(acc, Number(p.memoryTotalBytes)), memTotal);

  const loadValues = points.map((p) => (covered(p) ? p.load1 : null));
  const loadPeaks = points.map((p) => (covered(p) ? p.load1Peak : null));

  const diskReadBytes = points.map((p) => (covered(p) && !p.diskIoMissing ? Number(p.diskReadBytesPerSecond) : null));
  const diskWriteBytes = points.map((p) => (covered(p) && !p.diskIoMissing ? Number(p.diskWriteBytesPerSecond) : null));
  const diskReadIops = points.map((p) => (covered(p) && !p.diskIoMissing ? p.diskReadIops : null));
  const diskWriteIops = points.map((p) => (covered(p) && !p.diskIoMissing ? p.diskWriteIops : null));

  const netRx = points.map((p) => (covered(p) && !p.networkIoMissing ? Number(p.networkRxBytesPerSecond) : null));
  const netTx = points.map((p) => (covered(p) && !p.networkIoMissing ? Number(p.networkTxBytesPerSecond) : null));

  const rangeOptions: { value: RangeKey; label: string }[] = [
    { value: "1h", label: t`1 h` },
    { value: "6h", label: t`6 h` },
    { value: "24h", label: t`24 h` },
    { value: "7d", label: t`7 d` },
    { value: "30d", label: t`30 d` },
  ];

  const diskModeOptions: { value: "bytes" | "iops"; label: string }[] = [
    { value: "bytes", label: t`Throughput` },
    { value: "iops", label: t`IOPS` },
  ];

  return (
    <section className="srv-hist" aria-labelledby="srv-hist-title">
      <div className="srv-hist-bar">
        <h2 className="dw-text-heading" id="srv-hist-title">
          <Trans>History</Trans>
        </h2>
        <SegmentedControl
          size="sm"
          label={t`Time range`}
          options={rangeOptions}
          value={range}
          onChange={(next) => {
            setRange(next);
            setActiveIndex(null);
          }}
        />
      </div>

      {query.error ? (
        <ReadFailed
          error={query.error}
          title={t`History couldn't be loaded`}
          onRetry={() => void query.refetch()}
          retrying={query.isFetching}
        />
      ) : query.isPending ? (
        <Loading label={t`Loading server history`}>
          <div className="srv-hist-grid">
            <SkeletonCard title={t`CPU`} />
            <SkeletonCard title={t`Memory`} />
            <SkeletonCard title={t`Load`} />
            <SkeletonCard title={t`Disk I/O`} />
            <SkeletonCard title={t`Network`} />
          </div>
        </Loading>
      ) : emptyMsg ? (
        <Panel className="srv-hist-empty">
          <p className="srv-hist-empty-text">{emptyMsg}</p>
        </Panel>
      ) : (
        <div className="srv-hist-grid">
          <TimeSeriesChart
            title={t`CPU`}
            timestamps={timestamps}
            gaps={gaps}
            min={0}
            max={100}
            threshold={HIGH_USE}
            formatAxis={(v) => `${Math.round(v)}%`}
            formatTick={formatTick}
            formatHoverTime={formatHoverTime}
            gapLabel={gapLabel}
            keyboardHint={keyboardHint}
            emptyMessage={emptyMsg}
            activeIndex={activeIndex}
            onActiveIndexChange={setActiveIndex}
            series={[
              {
                id: "cpu",
                label: t`Avg`,
                values: cpuValues,
                peaks: cpuPeaks,
                tone: "ink",
                formatValue: (v) => `${formatFixed(v, 1, locale)}%`,
                formatPeak: (pk) => t`peak ${formatFixed(pk, 1, locale)}%`,
              },
            ]}
          />

          <TimeSeriesChart
            title={t`Memory`}
            timestamps={timestamps}
            gaps={gaps}
            min={0}
            max={maxMemTotal > 0 ? maxMemTotal : undefined}
            threshold={maxMemTotal > 0 ? (maxMemTotal * HIGH_USE) / 100 : undefined}
            formatAxis={(v) => formatBytes(Math.round(v), locale)}
            formatTick={formatTick}
            formatHoverTime={formatHoverTime}
            gapLabel={gapLabel}
            keyboardHint={keyboardHint}
            emptyMessage={emptyMsg}
            activeIndex={activeIndex}
            onActiveIndexChange={setActiveIndex}
            series={[
              {
                id: "memory",
                label: t`Used`,
                values: memValues,
                peaks: memPeaks,
                tone: "cobalt",
                formatValue: (v) => formatBytes(Math.round(v), locale),
                formatPeak: (pk) => t`peak ${formatBytes(Math.round(pk), locale)}`,
              },
            ]}
          />

          <TimeSeriesChart
            title={t`Load`}
            timestamps={timestamps}
            gaps={gaps}
            min={0}
            threshold={cpus > 0 ? cpus : undefined}
            formatAxis={(v) => formatFixed(v, 1, locale)}
            formatTick={formatTick}
            formatHoverTime={formatHoverTime}
            gapLabel={gapLabel}
            keyboardHint={keyboardHint}
            emptyMessage={emptyMsg}
            activeIndex={activeIndex}
            onActiveIndexChange={setActiveIndex}
            series={[
              {
                id: "load",
                label: t`1 min`,
                values: loadValues,
                peaks: loadPeaks,
                tone: "ink",
                formatValue: (v) => formatFixed(v, 2, locale),
                formatPeak: (pk) => t`peak ${formatFixed(pk, 2, locale)}`,
              },
            ]}
          />

          <TimeSeriesChart
            title={t`Disk I/O`}
            controls={
              <div className="srv-hist-chart-controls">
                {disks.length > 1 ? (
                  <SelectField
                    size="sm"
                    mono
                    className="srv-hist-select-field"
                    aria-label={t`Disk`}
                    value={disk}
                    onChange={(e) => setDisk(e.target.value)}
                    options={[{ value: "", label: t`All disks` }, ...disks.map((d) => ({ value: d, label: d }))]}
                  />
                ) : disks.length === 1 ? (
                  <code className="srv-hist-tag">{disks[0]}</code>
                ) : null}
                <SegmentedControl
                  size="sm"
                  label={t`Disk I/O unit`}
                  options={diskModeOptions}
                  value={diskMode}
                  onChange={setDiskMode}
                />
              </div>
            }
            timestamps={timestamps}
            gaps={gaps}
            min={0}
            formatAxis={(v) => (diskMode === "bytes" ? formatByteRate(v, locale) : formatIops(v, locale))}
            formatTick={formatTick}
            formatHoverTime={formatHoverTime}
            gapLabel={gapLabel}
            keyboardHint={keyboardHint}
            emptyMessage={emptyMsg}
            activeIndex={activeIndex}
            onActiveIndexChange={setActiveIndex}
            series={
              diskMode === "bytes"
                ? [
                    {
                      id: "disk-read",
                      label: t`Read`,
                      values: diskReadBytes,
                      tone: "cobalt",
                      formatValue: (v) => formatByteRate(v, locale),
                    },
                    {
                      id: "disk-write",
                      label: t`Write`,
                      values: diskWriteBytes,
                      tone: "warning",
                      formatValue: (v) => formatByteRate(v, locale),
                    },
                  ]
                : [
                    {
                      id: "disk-read-iops",
                      label: t`Read`,
                      values: diskReadIops,
                      tone: "cobalt",
                      formatValue: (v) => formatIops(v, locale),
                    },
                    {
                      id: "disk-write-iops",
                      label: t`Write`,
                      values: diskWriteIops,
                      tone: "warning",
                      formatValue: (v) => formatIops(v, locale),
                    },
                  ]
            }
          />

          <TimeSeriesChart
            className="srv-hist-wide"
            title={t`Network`}
            controls={
              interfaces.length > 1 ? (
                <SelectField
                  size="sm"
                  mono
                  className="srv-hist-select-field"
                  aria-label={t`Network interface`}
                  value={iface}
                  onChange={(e) => setIface(e.target.value)}
                  options={[
                    { value: "", label: t`All interfaces` },
                    ...interfaces.map((name) => ({ value: name, label: name })),
                  ]}
                />
              ) : interfaces.length === 1 ? (
                <code className="srv-hist-tag">{interfaces[0]}</code>
              ) : null
            }
            timestamps={timestamps}
            gaps={gaps}
            min={0}
            formatAxis={(v) => formatBitRate(Math.round(v), locale).join(" ")}
            formatTick={formatTick}
            formatHoverTime={formatHoverTime}
            gapLabel={gapLabel}
            keyboardHint={keyboardHint}
            emptyMessage={emptyMsg}
            activeIndex={activeIndex}
            onActiveIndexChange={setActiveIndex}
            series={[
              {
                id: "net-rx",
                label: t`In`,
                values: netRx,
                tone: "cobalt",
                formatValue: (v) => formatBitRate(Math.round(v), locale).join(" "),
              },
              {
                id: "net-tx",
                label: t`Out`,
                values: netTx,
                tone: "ink",
                dashed: true,
                formatValue: (v) => formatBitRate(Math.round(v), locale).join(" "),
              },
            ]}
          />
        </div>
      )}
    </section>
  );
}
