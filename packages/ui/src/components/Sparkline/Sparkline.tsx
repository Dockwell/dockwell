// SPDX-License-Identifier: AGPL-3.0-only

import { cx } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import "./Sparkline.css";

export type SparklineTone = "ink" | "cobalt" | "healthy" | "warning" | "critical" | "idle";

export interface SparklineProps {
  /**
   * The series, oldest first. null is a gap, where nothing was heard: the line breaks there rather
   * than draw a zero, so an outage never reads as a calm stretch.
   */
  data: readonly (number | null)[];
  /**
   * The highest value at each point, beside data's averages: drawn as a light band from the line up
   * to it, so a point that averaged 20 and touched 100 doesn't read as a calm one. null where data is.
   */
  peaks?: readonly (number | null)[];
  /** Pixels or a CSS length such as "100%". The line stretches to fit. */
  width?: number | string;
  height?: number;
  /** ink by default; cobalt for live traffic; a status tone only when the series itself is in that state. */
  tone?: SparklineTone;
  /** Set min and max when values have a known range (0 and 100 for percentages), so rows compare. */
  min?: number;
  max?: number;
  /** Dashed warning line at this value. */
  threshold?: number;
  /** Fills under the line. On by default. */
  area?: boolean;
  /** Accessible name. Sparklines show shape, not values, so say what the series is and where it ends. */
  label?: string;
  /** Names the sparkline by text already on the page instead, such as the label of the metric it sits under. */
  "aria-labelledby"?: string;
  className?: string;
}

const W = 100;
const PAD = 2;

/** The runs of points with a value, each as [index, value] pairs: a gap ends one. */
function runsOf(data: readonly (number | null)[]): [number, number][][] {
  const runs: [number, number][][] = [];
  let run: [number, number][] = [];
  data.forEach((v, i) => {
    if (v == null || !Number.isFinite(v)) {
      if (run.length > 0) runs.push(run);
      run = [];
      return;
    }
    run.push([i, v]);
  });
  if (run.length > 0) runs.push(run);
  return runs;
}

/** A small line showing the shape of a series, without axes. Put the number beside it. */
export function Sparkline({
  data,
  peaks,
  width = 120,
  height = 32,
  tone = "ink",
  min,
  max,
  threshold,
  area = true,
  label,
  "aria-labelledby": labelledBy,
  className,
}: SparklineProps) {
  const strings = useUIStrings();
  const heard = data.filter((v): v is number => v != null && Number.isFinite(v));
  const highs = (peaks ?? []).filter((v): v is number => v != null && Number.isFinite(v));
  // One sample, or none, is a flat line across: a series of one has no shape to show.
  const series: readonly (number | null)[] = data.length >= 2 ? data : [data[0] ?? 0, data[0] ?? 0];
  const scale = heard.length > 0 ? [...heard, ...highs] : [0];
  let lo = min ?? Math.min(...scale);
  let hi = max ?? Math.max(...scale);
  if (threshold != null) {
    lo = Math.min(lo, threshold);
    hi = Math.max(hi, threshold);
  }
  if (hi === lo) {
    hi += 1;
    lo -= 1;
  }
  const y = (v: number) => PAD + (1 - (v - lo) / (hi - lo)) * (height - PAD * 2);
  const x = (i: number) => (i / (series.length - 1)) * W;
  const at = ([i, v]: [number, number]) => `${x(i).toFixed(2)} ${y(v).toFixed(2)}`;
  const runs = runsOf(series);
  // A run of one point is a dot, as a round cap on a line of no length.
  const line = runs.map((run) => `M${run.map(at).join("L")}${run.length === 1 ? "h0" : ""}`).join("");
  const areas = runs.map(
    (run) =>
      `M${x(run[0]?.[0] ?? 0).toFixed(2)} ${height}L${run.map(at).join("L")}L${x(run[run.length - 1]?.[0] ?? 0).toFixed(2)} ${height}Z`,
  );
  // The band from each point's average up to its peak, along each run.
  const bands = peaks
    ? runs.flatMap((run) => {
        const tops = run.map(([i, v]): [number, number] => [i, Math.max(peaks[i] ?? v, v)]);
        if (tops.every(([i, top]) => top === series[i])) return [];
        return [`M${tops.map(at).join("L")}L${[...run].reverse().map(at).join("L")}Z`];
      })
    : [];
  const latest = heard[heard.length - 1] ?? 0;
  return (
    <svg
      className={cx("dw-spark", `dw-tone-${tone}`, className)}
      viewBox={`0 0 ${W} ${height}`}
      preserveAspectRatio="none"
      width={width}
      height={height}
      role="img"
      aria-label={labelledBy ? undefined : (label ?? strings.sparkline(heard.length, latest))}
      aria-labelledby={labelledBy}
      focusable="false"
    >
      {area ? areas.map((d) => <path key={d} d={d} className="dw-spark-area" />) : null}
      {bands.map((d) => (
        <path key={d} d={d} className="dw-spark-peak" />
      ))}
      {threshold != null ? (
        <line
          x1={0}
          x2={W}
          y1={y(threshold)}
          y2={y(threshold)}
          className="dw-spark-threshold"
          vectorEffect="non-scaling-stroke"
        />
      ) : null}
      {line ? <path d={line} className="dw-spark-line" vectorEffect="non-scaling-stroke" /> : null}
    </svg>
  );
}
