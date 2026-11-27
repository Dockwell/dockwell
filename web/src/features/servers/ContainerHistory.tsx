// SPDX-License-Identifier: AGPL-3.0-only

import { type Timestamp, timestampMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { type ContainerHistoryPoint, HistoryPointState, ServerService } from "@dockwell/api";
import { Callout, Loading, SegmentedControl, SkeletonCard, TimeSeriesChart } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type CSSProperties, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatFixed } from "../../lib/format";
import { useMay } from "../auth/may";
import {
  axisWidthCh,
  emptyReason,
  gapKindOf,
  RANGE_TO_PROTO,
  type RangeKey,
  useHistoryFormatters,
} from "./ServerHistory";
import "./ServerHistory.css";

/**
 * Historical CPU and memory charts for a single container across 1 h to 30 d,
 * with synchronized hover across both charts.
 */
export function ContainerHistory({
  serverId,
  containerId,
  memoryLimitBytes,
  createdAt,
  stopped,
}: {
  serverId: string;
  containerId: string;
  memoryLimitBytes?: bigint | undefined;
  createdAt?: Timestamp | undefined;
  stopped?: boolean | undefined;
}) {
  const { t } = useLingui();
  const may = useMay();
  const allowed = may(ServerService.method.getContainerHistory).allowed;
  const valid = /^[0-9a-f]{64}$/.test(containerId);
  const [range, setRange] = useState<RangeKey>("24h");
  const [activeIndex, setActiveIndex] = useState<number | null>(null);

  const query = useQuery(
    ServerService.method.getContainerHistory,
    { serverId, containerId, range: RANGE_TO_PROTO[range], createdAt },
    { enabled: allowed && valid, retry: false, refetchInterval: 60_000, refetchOnWindowFocus: false },
  );

  const { locale, formatTick, formatHoverTime, gapLabel, keyboardHint } = useHistoryFormatters(range);

  const points: readonly ContainerHistoryPoint[] = query.data?.points ?? [];
  const displayStates = useMemo(
    () =>
      points.map((p) => ({
        state: stopped && p.state === HistoryPointState.NOT_TRACKED ? HistoryPointState.NOT_RUNNING : p.state,
      })),
    [points, stopped],
  );
  const timestamps = useMemo(() => points.map((p) => (p.time ? timestampMs(p.time) : 0)), [points]);
  const gaps = useMemo(() => displayStates.map((p) => gapKindOf(p.state)), [displayStates]);
  const emptyMsg = useMemo(
    () => emptyReason(displayStates, gapLabel, t`No container samples recorded for this range yet.`),
    [displayStates, gapLabel, t],
  );

  if (!allowed || !valid) return null;

  const covered = (p: ContainerHistoryPoint) => p.state === HistoryPointState.COVERED;
  const cpuValues = points.map((p) => (covered(p) ? p.cpuPercent : null));
  const cpuPeaks = points.map((p) => (covered(p) ? p.cpuPeakPercent : null));
  const memValues = points.map((p) => (covered(p) ? Number(p.memoryBytes) : null));
  const memPeaks = points.map((p) => (covered(p) ? Number(p.memoryPeakBytes) : null));
  const limit = points.reduce((acc, p) => Math.max(acc, Number(p.memoryLimitBytes)), Number(memoryLimitBytes ?? 0n));
  const peakCpu = Math.max(1, ...cpuValues.map((v) => v ?? 0), ...cpuPeaks.map((v) => v ?? 0));
  const peakMem = limit > 0 ? limit : Math.max(1, ...memValues.map((v) => v ?? 0), ...memPeaks.map((v) => v ?? 0));
  const formatCpuAxis = (v: number) => `${formatFixed(v, v < 10 ? 1 : 0, locale)}%`;
  const formatMemAxis = (v: number) => formatBytes(Math.round(v), locale);
  const yAxisWidth = axisWidthCh([
    formatCpuAxis(peakCpu),
    formatCpuAxis(peakCpu / 2),
    formatMemAxis(peakMem),
    formatMemAxis(peakMem / 2),
  ]);

  const rangeOptions: { value: RangeKey; label: string }[] = [
    { value: "1h", label: t`1 h` },
    { value: "6h", label: t`6 h` },
    { value: "24h", label: t`24 h` },
    { value: "7d", label: t`7 d` },
    { value: "30d", label: t`30 d` },
  ];

  return (
    <section className="srv-hist" aria-labelledby="container-hist-title">
      <div className="srv-hist-bar">
        <h2 className="dw-text-heading" id="container-hist-title">
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
        <Callout tone="warning" title={t`Container history couldn't be loaded`}>
          {sentence(query.error)}
        </Callout>
      ) : query.isPending ? (
        <Loading label={t`Loading container history`}>
          <div className="srv-hist-grid">
            <SkeletonCard title={t`CPU`} />
            <SkeletonCard title={t`Memory`} />
          </div>
        </Loading>
      ) : (
        <div className="srv-hist-grid" style={{ "--dw-ts-yaxis": `${yAxisWidth}ch` } as CSSProperties}>
          <TimeSeriesChart
            title={t`CPU`}
            timestamps={timestamps}
            gaps={gaps}
            min={0}
            formatAxis={formatCpuAxis}
            formatTick={formatTick}
            formatHoverTime={formatHoverTime}
            gapLabel={gapLabel}
            keyboardHint={keyboardHint}
            emptyMessage={emptyMsg}
            activeIndex={activeIndex}
            onActiveIndexChange={setActiveIndex}
            series={[
              {
                id: "container-cpu",
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
            max={limit > 0 ? limit : undefined}
            threshold={limit > 0 ? limit * 0.9 : undefined}
            formatAxis={formatMemAxis}
            formatTick={formatTick}
            formatHoverTime={formatHoverTime}
            gapLabel={gapLabel}
            keyboardHint={keyboardHint}
            emptyMessage={emptyMsg}
            activeIndex={activeIndex}
            onActiveIndexChange={setActiveIndex}
            series={[
              {
                id: "container-memory",
                label: t`Used`,
                values: memValues,
                peaks: memPeaks,
                tone: "cobalt",
                formatValue: (v) => formatBytes(Math.round(v), locale),
                formatPeak: (pk) => t`peak ${formatBytes(Math.round(pk), locale)}`,
              },
            ]}
          />
        </div>
      )}
    </section>
  );
}
