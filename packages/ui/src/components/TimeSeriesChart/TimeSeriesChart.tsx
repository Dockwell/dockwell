// SPDX-License-Identifier: AGPL-3.0-only

import { type KeyboardEvent, type PointerEvent, type ReactNode, useId, useMemo, useState } from "react";

import { cx } from "../../lib/cx";
import "./TimeSeriesChart.css";

export type TimeSeriesTone = "ink" | "cobalt" | "healthy" | "warning" | "critical" | "muted";
export type TimeSeriesGapKind =
  | "offline"
  | "before-join"
  | "no-control-plane"
  | "not-running"
  | "before-create"
  | "not-tracked";

export interface TimeSeriesLine {
  id: string;
  label: string;
  /** Oldest first; null denotes a gap where the line breaks. */
  values: readonly (number | null)[];
  /** Optional peak values per bucket, drawn as a light band above values. */
  peaks?: readonly (number | null)[];
  tone?: TimeSeriesTone;
  dashed?: boolean;
  /** Formats a value for the synchronized hover readout. */
  formatValue: (value: number) => string;
  /** Formats a peak value when different from average. */
  formatPeak?: (peak: number) => string;
}

export interface TimeSeriesChartProps {
  /** Chart title shown top-left (sentence case). */
  title: string;
  /** Optional controls in the header right (e.g. disk or interface picker, throughput/IOPS switch). */
  controls?: ReactNode;
  /** Timestamp (epoch ms) for each bucket index, oldest first. */
  timestamps: readonly number[];
  /** Gap classification for each bucket index (null when covered). */
  gaps?: readonly (TimeSeriesGapKind | null)[];
  /** One or more lines to draw on the shared Y axis. */
  series: readonly TimeSeriesLine[];
  /** Known Y-axis lower bound (defaults to 0). */
  min?: number;
  /** Known Y-axis upper bound (auto-scales from data when omitted). */
  max?: number;
  /** Optional dashed reference/warning threshold line. */
  threshold?: number;
  /** Formats Y-axis tick labels. */
  formatAxis: (value: number) => string;
  /** Formats X-axis tick labels from epoch ms. */
  formatTick: (timestampMs: number) => string;
  /** Formats the hovered bucket's timestamp for the synchronized readout. */
  formatHoverTime: (timestampMs: number) => string;
  /** Plain-language message for each gap kind, shown on hover and empty states. */
  gapLabel?: (kind: TimeSeriesGapKind) => string;
  /** Shown when there are no covered points in the entire range. */
  emptyMessage?: string;
  /** Chart plot height in CSS pixels. Defaults to 148. */
  height?: number;
  /** Shared hovered bucket index across synchronized charts. */
  activeIndex: number | null;
  /** Updates the shared hovered bucket index across synchronized charts. */
  onActiveIndexChange: (index: number | null) => void;
  /** Screen-reader hint explaining keyboard navigation (defaults to English). */
  keyboardHint?: string;
  className?: string;
}

const VB_W = 480;
const PAD_L = 4;
const PAD_R = 4;
const PAD_T = 8;
const PAD_B = 6;

function runsOf(values: readonly (number | null)[]): [number, number][][] {
  const runs: [number, number][][] = [];
  let run: [number, number][] = [];
  for (let i = 0; i < values.length; i++) {
    const v = values[i];
    if (v == null || !Number.isFinite(v)) {
      if (run.length > 0) runs.push(run);
      run = [];
      continue;
    }
    run.push([i, v]);
  }
  if (run.length > 0) runs.push(run);
  return runs;
}

interface GapSpan {
  start: number;
  end: number;
  kind: TimeSeriesGapKind;
}

function gapSpansOf(gaps: readonly (TimeSeriesGapKind | null)[] | undefined): GapSpan[] {
  if (!gaps || gaps.length === 0) return [];
  const spans: GapSpan[] = [];
  let cur: GapSpan | null = null;
  for (let i = 0; i < gaps.length; i++) {
    const k = gaps[i] ?? null;
    if (k === null) {
      if (cur) spans.push(cur);
      cur = null;
      continue;
    }
    if (cur && cur.kind === k) {
      cur.end = i;
    } else {
      if (cur) spans.push(cur);
      cur = { start: i, end: i, kind: k };
    }
  }
  if (cur) spans.push(cur);
  return spans;
}

function defaultGapLabel(kind: TimeSeriesGapKind): string {
  switch (kind) {
    case "before-join":
      return "No data before Dockwell joined";
    case "offline":
      return "The agent was offline";
    case "no-control-plane":
      return "The control plane was offline";
    case "not-running":
      return "The container wasn't running";
    case "before-create":
      return "The container wasn't created yet";
    case "not-tracked":
      return "Not tracked (past the 128-container limit)";
  }
}

const DEFAULT_KEYBOARD_HINT = "Use the left and right arrows to read values over time.";

/**
 * Interactive SVG time-series chart with synchronized crosshair hover across
 * sibling charts, peak bands, and explicit gap shading. Built directly on
 * Dockwell's design tokens with zero external chart dependencies.
 */
export function TimeSeriesChart({
  title,
  controls,
  timestamps,
  gaps,
  series,
  min = 0,
  max,
  threshold,
  formatAxis,
  formatTick,
  formatHoverTime,
  gapLabel = defaultGapLabel,
  emptyMessage,
  height = 148,
  activeIndex,
  onActiveIndexChange,
  keyboardHint = DEFAULT_KEYBOARD_HINT,
  className,
}: TimeSeriesChartProps) {
  const titleId = useId();
  const descId = useId();
  const [focused, setFocused] = useState(false);
  const count = timestamps.length;

  const { lo, hi, hasData } = useMemo(() => {
    let dataMax = Number.NEGATIVE_INFINITY;
    let any = false;
    for (const s of series) {
      for (const v of s.values) {
        if (v != null && Number.isFinite(v)) {
          any = true;
          if (v > dataMax) dataMax = v;
        }
      }
      if (s.peaks) {
        for (const p of s.peaks) {
          if (p != null && Number.isFinite(p) && p > dataMax) dataMax = p;
        }
      }
    }
    let upper = max ?? (any ? dataMax * 1.1 : 1);
    if (threshold != null && max == null) upper = Math.max(upper, threshold * 1.05);
    if (!Number.isFinite(upper) || upper <= min) upper = min + 1;
    return { lo: min, hi: upper, hasData: any };
  }, [series, min, max, threshold]);

  const plotW = VB_W - PAD_L - PAD_R;
  const plotH = height - PAD_T - PAD_B;
  const xAt = (i: number) => (count <= 1 ? PAD_L + plotW / 2 : PAD_L + (i / (count - 1)) * plotW);
  const yAt = (v: number) => {
    const clamped = Math.max(lo, Math.min(hi, v));
    return PAD_T + (1 - (clamped - lo) / (hi - lo)) * plotH;
  };

  const gapSpans = useMemo(() => gapSpansOf(gaps), [gaps]);

  const validActive = activeIndex != null && activeIndex >= 0 && activeIndex < count ? activeIndex : null;
  const activeGap = validActive != null ? (gaps?.[validActive] ?? null) : null;
  const activeTime = validActive != null ? timestamps[validActive] : undefined;

  // Pick up to 4 evenly-spaced X-axis ticks without duplicates when count < 4.
  const xTicks = useMemo(() => {
    if (count === 0) return [];
    if (count === 1) return [{ i: 0, label: formatTick(timestamps[0] ?? 0) }];
    const steps = Math.min(3, count - 1);
    const out: { i: number; label: string }[] = [];
    for (let s = 0; s <= steps; s++) {
      const idx = Math.round((s / steps) * (count - 1));
      if (out.length === 0 || out[out.length - 1]?.i !== idx) {
        out.push({ i: idx, label: formatTick(timestamps[idx] ?? 0) });
      }
    }
    return out;
  }, [count, timestamps, formatTick]);

  const handlePointer = (e: PointerEvent<SVGSVGElement>) => {
    if (count === 0) return;
    const rect = e.currentTarget.getBoundingClientRect();
    if (rect.width <= 0) return;
    const rel = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    const idx = Math.round(rel * (count - 1));
    if (idx !== activeIndex) onActiveIndexChange(idx);
  };

  const handleKeyDown = (e: KeyboardEvent<SVGSVGElement>) => {
    if (count === 0) return;
    if (e.key === "ArrowRight") {
      e.preventDefault();
      onActiveIndexChange(validActive == null ? 0 : Math.min(count - 1, validActive + 1));
    } else if (e.key === "ArrowLeft") {
      e.preventDefault();
      onActiveIndexChange(validActive == null ? count - 1 : Math.max(0, validActive - 1));
    } else if (e.key === "Escape") {
      onActiveIndexChange(null);
    }
  };

  // Compute summary readout (either hovered bucket or latest covered bucket).
  const latestIndex = useMemo(() => {
    for (let i = count - 1; i >= 0; i--) {
      if (series.some((s) => s.values[i] != null)) return i;
    }
    return null;
  }, [count, series]);

  const shownIndex = validActive ?? latestIndex;

  // Short accessible text summary (average, peak, and keyboard hint) for screen readers.
  const description = useMemo(() => {
    if (!hasData) {
      return emptyMessage ?? gapLabel("before-join");
    }
    const parts: string[] = [];
    for (const s of series) {
      let sum = 0;
      let n = 0;
      let peak = Number.NEGATIVE_INFINITY;
      for (let i = 0; i < s.values.length; i++) {
        const v = s.values[i];
        if (v != null && Number.isFinite(v)) {
          sum += v;
          n++;
          if (v > peak) peak = v;
        }
        const pk = s.peaks?.[i];
        if (pk != null && Number.isFinite(pk) && pk > peak) peak = pk;
      }
      if (n > 0) {
        const avg = s.formatValue(sum / n);
        const hiStr = s.formatPeak ? s.formatPeak(peak) : s.formatValue(peak);
        parts.push(`${s.label} ${avg}, ${hiStr}`);
      }
    }
    const summary = parts.length > 0 ? `${parts.join("; ")}.` : "";
    return summary ? `${summary} ${keyboardHint}` : keyboardHint;
  }, [hasData, emptyMessage, gapLabel, series, keyboardHint]);

  const yTicks = useMemo(() => {
    const top = formatAxis(hi);
    const mid = formatAxis(lo + (hi - lo) / 2);
    const bot = formatAxis(lo);
    if (mid === top || mid === bot) return [top, bot];
    return [top, mid, bot];
  }, [formatAxis, hi, lo]);

  return (
    <div className={cx("dw-panel dw-ts", className)}>
      <div className="dw-ts-head">
        <div className="dw-ts-title-row">
          <h3 className="dw-ts-title" id={titleId}>
            {title}
          </h3>
          {controls ? <div className="dw-ts-controls">{controls}</div> : null}
        </div>
        <div className="dw-ts-readout" aria-live={focused ? "polite" : "off"}>
          {validActive != null && activeTime != null ? (
            <>
              <span className="dw-ts-time">{formatHoverTime(activeTime)}</span>
              {activeGap ? (
                <span className="dw-ts-gap-word">{gapLabel(activeGap)}</span>
              ) : (
                <span className="dw-ts-values">
                  {series.map((s) => {
                    const v = s.values[validActive];
                    const pk = s.peaks?.[validActive];
                    const showPeak = pk != null && v != null && pk > v * 1.05 && s.formatPeak;
                    return (
                      <span key={s.id} className={cx("dw-ts-val", `dw-tone-${s.tone ?? "ink"}`)}>
                        <span className="dw-ts-swatch" aria-hidden="true" />
                        <span className="dw-ts-val-label">{s.label}</span>
                        <span className="dw-ts-val-num">{v != null ? s.formatValue(v) : "–"}</span>
                        {showPeak && s.formatPeak ? <span className="dw-ts-val-peak">({s.formatPeak(pk)})</span> : null}
                      </span>
                    );
                  })}
                </span>
              )}
            </>
          ) : shownIndex != null ? (
            <span className="dw-ts-values">
              {series.map((s) => {
                const v = s.values[shownIndex];
                return (
                  <span key={s.id} className={cx("dw-ts-val", `dw-tone-${s.tone ?? "ink"}`)}>
                    <span className="dw-ts-swatch" aria-hidden="true" />
                    <span className="dw-ts-val-label">{s.label}</span>
                    <span className="dw-ts-val-num">{v != null ? s.formatValue(v) : "–"}</span>
                  </span>
                );
              })}
            </span>
          ) : emptyMessage ? (
            <span className="dw-ts-gap-word" aria-hidden="true">
              –
            </span>
          ) : (
            <span className="dw-ts-gap-word">{gapLabel("before-join")}</span>
          )}
        </div>
      </div>

      <div className="dw-ts-body">
        <div className="dw-ts-yaxis" aria-hidden="true">
          {yTicks.map((label, idx) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: fixed 2-3 tick slots from top to bottom
            <span key={idx}>{label}</span>
          ))}
        </div>
        <div className="dw-ts-plot">
          {!hasData && emptyMessage ? (
            <div className="dw-ts-empty" style={{ height }}>
              <span>{emptyMessage}</span>
            </div>
          ) : (
            <svg
              className="dw-ts-svg"
              viewBox={`0 0 ${VB_W} ${height}`}
              preserveAspectRatio="none"
              style={{ height }}
              role="img"
              // biome-ignore lint/a11y/noNoninteractiveTabindex: chart supports ArrowLeft/ArrowRight bucket inspection
              tabIndex={0}
              aria-labelledby={titleId}
              aria-describedby={descId}
              onFocus={() => setFocused(true)}
              onBlur={() => setFocused(false)}
              onPointerMove={handlePointer}
              onPointerEnter={handlePointer}
              onPointerLeave={() => onActiveIndexChange(null)}
              onKeyDown={handleKeyDown}
            >
              <title>{title}</title>
              <desc id={descId}>{description}</desc>
              {/* Horizontal gridlines */}
              <line
                x1={PAD_L}
                x2={VB_W - PAD_R}
                y1={PAD_T}
                y2={PAD_T}
                className="dw-ts-grid"
                vectorEffect="non-scaling-stroke"
              />
              <line
                x1={PAD_L}
                x2={VB_W - PAD_R}
                y1={PAD_T + plotH / 2}
                y2={PAD_T + plotH / 2}
                className="dw-ts-grid"
                vectorEffect="non-scaling-stroke"
              />
              <line
                x1={PAD_L}
                x2={VB_W - PAD_R}
                y1={PAD_T + plotH}
                y2={PAD_T + plotH}
                className="dw-ts-grid"
                vectorEffect="non-scaling-stroke"
              />

              {/* Gap bands */}
              {gapSpans.map((g) => {
                const x1 = xAt(g.start);
                const x2 = xAt(g.end);
                const step = count > 1 ? plotW / (count - 1) : plotW;
                const left = Math.max(PAD_L, x1 - step / 2);
                const right = Math.min(VB_W - PAD_R, x2 + step / 2);
                return (
                  <rect
                    key={`${g.kind}-${g.start}`}
                    x={left}
                    y={PAD_T}
                    width={Math.max(right - left, 1)}
                    height={plotH}
                    className={cx("dw-ts-gap", `dw-ts-gap-${g.kind}`)}
                  />
                );
              })}

              {/* Threshold line */}
              {threshold != null && threshold >= lo && threshold <= hi ? (
                <line
                  x1={PAD_L}
                  x2={VB_W - PAD_R}
                  y1={yAt(threshold)}
                  y2={yAt(threshold)}
                  className="dw-ts-threshold"
                  vectorEffect="non-scaling-stroke"
                />
              ) : null}

              {/* Series lines, areas, and peak bands */}
              {series.map((s) => {
                const tone = s.tone ?? "ink";
                const runs = runsOf(s.values);
                const at = ([i, v]: [number, number]) => `${xAt(i).toFixed(2)} ${yAt(v).toFixed(2)}`;
                const lineD = runs.map((run) => `M${run.map(at).join("L")}${run.length === 1 ? "h0" : ""}`).join("");
                const areaD =
                  series.length === 1
                    ? runs.map(
                        (run) =>
                          `M${xAt(run[0]?.[0] ?? 0).toFixed(2)} ${(PAD_T + plotH).toFixed(2)}L${run.map(at).join("L")}L${xAt(run[run.length - 1]?.[0] ?? 0).toFixed(2)} ${(PAD_T + plotH).toFixed(2)}Z`,
                      )
                    : [];
                const peakBands = s.peaks
                  ? runs.flatMap((run) => {
                      const peaks = s.peaks;
                      if (!peaks) return [];
                      const tops = run.map(([i, v]): [number, number] => [i, Math.max(peaks[i] ?? v, v)]);
                      if (tops.every(([i, top]) => top === s.values[i])) return [];
                      return [`M${tops.map(at).join("L")}L${[...run].reverse().map(at).join("L")}Z`];
                    })
                  : [];
                return (
                  <g key={s.id} className={cx("dw-ts-series", `dw-tone-${tone}`)}>
                    {areaD.map((d) => (
                      <path key={d} d={d} className="dw-ts-area" />
                    ))}
                    {peakBands.map((d) => (
                      <path key={d} d={d} className="dw-ts-peak" />
                    ))}
                    {lineD ? (
                      <path
                        d={lineD}
                        className={cx("dw-ts-line", s.dashed && "is-dashed")}
                        vectorEffect="non-scaling-stroke"
                      />
                    ) : null}
                  </g>
                );
              })}

              {/* Synchronized crosshair rule */}
              {validActive != null ? (
                <line
                  x1={xAt(validActive)}
                  x2={xAt(validActive)}
                  y1={PAD_T}
                  y2={PAD_T + plotH}
                  className="dw-ts-crosshair"
                  vectorEffect="non-scaling-stroke"
                />
              ) : null}
            </svg>
          )}
          {/* Crisp non-stretched dots at the hovered timestamp */}
          {hasData && validActive != null ? (
            <div className="dw-ts-dots" aria-hidden="true">
              {series.map((s) => {
                const v = s.values[validActive];
                if (v == null || !Number.isFinite(v)) return null;
                const leftPct = count <= 1 ? 50 : (validActive / (count - 1)) * 100;
                const topPx = yAt(v);
                return (
                  <span
                    key={s.id}
                    className={cx("dw-ts-dot", `dw-tone-${s.tone ?? "ink"}`)}
                    style={{ left: `${leftPct}%`, top: `${topPx}px` }}
                  />
                );
              })}
            </div>
          ) : null}
        </div>
        {xTicks.length > 0 ? (
          <div className="dw-ts-xaxis" aria-hidden="true">
            {xTicks.map((tick) => (
              <span key={tick.i}>{tick.label}</span>
            ))}
          </div>
        ) : null}
      </div>
    </div>
  );
}
