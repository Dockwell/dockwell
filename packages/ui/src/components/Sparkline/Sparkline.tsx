// SPDX-License-Identifier: AGPL-3.0-only

import { cx } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import "./Sparkline.css";

export type SparklineTone = "ink" | "cobalt" | "healthy" | "warning" | "critical" | "idle";

export interface SparklineProps {
  data: readonly number[];
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

/** A small line showing the shape of a series, without axes. Put the number beside it. */
export function Sparkline({
  data,
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
  const first = data[0] ?? 0;
  const values = data.length >= 2 ? data : [first, first];
  let lo = min ?? Math.min(...values);
  let hi = max ?? Math.max(...values);
  if (threshold != null) {
    lo = Math.min(lo, threshold);
    hi = Math.max(hi, threshold);
  }
  if (hi === lo) {
    hi += 1;
    lo -= 1;
  }
  const y = (v: number) => PAD + (1 - (v - lo) / (hi - lo)) * (height - PAD * 2);
  const points = values.map((v, i) => `${((i / (values.length - 1)) * W).toFixed(2)} ${y(v).toFixed(2)}`);
  const line = `M${points.join("L")}`;
  const latest = data[data.length - 1] ?? 0;
  return (
    <svg
      className={cx("dw-spark", `dw-tone-${tone}`, className)}
      viewBox={`0 0 ${W} ${height}`}
      preserveAspectRatio="none"
      width={width}
      height={height}
      role="img"
      aria-label={labelledBy ? undefined : (label ?? strings.sparkline(data.length, latest))}
      aria-labelledby={labelledBy}
      focusable="false"
    >
      {area ? <path d={`${line}L${W} ${height}L0 ${height}Z`} className="dw-spark-area" /> : null}
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
      <path d={line} className="dw-spark-line" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}
