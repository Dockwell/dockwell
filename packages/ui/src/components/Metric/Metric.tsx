// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, useId } from "react";

import { cx, hasContent } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import { Sparkline, type SparklineTone } from "../Sparkline/Sparkline";
import "./Metric.css";

export interface MetricProps {
  label: ReactNode;
  /** Formatted and short: "11.2k", "38.4". Round to what a person can act on. */
  value: ReactNode;
  /** "%", "/min", "of 8 GB". */
  unit?: ReactNode;
  icon?: IconName;
  delta?: ReactNode;
  /** Only when the change is a problem or a recovery. A neutral change stays muted. */
  deltaTone?: "healthy" | "warning" | "critical";
  /** The recent series, drawn as a sparkline; null is a gap, where nothing was heard. */
  data?: readonly (number | null)[];
  /** Each point's highest beside data's averages, drawn as a light band above the line. */
  peaks?: readonly (number | null)[];
  tone?: SparklineTone;
  threshold?: number;
  min?: number;
  max?: number;
  /** What the number covers: "Average over the last hour". */
  caption?: ReactNode;
  className?: string;
}

/** One live number with its unit, its recent shape and a line of context. At most four in a row. */
export function Metric({
  label,
  value,
  unit,
  icon,
  delta,
  deltaTone,
  data,
  peaks,
  tone,
  threshold,
  min,
  max,
  caption,
  className,
}: MetricProps) {
  const strings = useUIStrings();
  const labelId = useId();
  const trendId = useId();
  return (
    <div className={cx("dw-metric", className)}>
      <div className="dw-metric-label">
        {icon ? <Icon name={icon} size={14} /> : null}
        <span id={labelId}>{label}</span>
      </div>
      <div className="dw-metric-row">
        <span className="dw-metric-value">{value}</span>
        {hasContent(unit) ? <span className="dw-metric-unit">{unit}</span> : null}
        {hasContent(delta) ? (
          <span className={cx("dw-metric-delta", deltaTone && `dw-tone-${deltaTone}`)}>{delta}</span>
        ) : null}
      </div>
      {data ? (
        <>
          {/* The sparkline is named by the label it sits under, whatever the label holds, and the word after it. */}
          <span id={trendId} hidden>
            {strings.trend}
          </span>
          <Sparkline
            data={data}
            peaks={peaks}
            tone={tone}
            threshold={threshold}
            width="100%"
            height={36}
            min={min}
            max={max}
            aria-labelledby={`${labelId} ${trendId}`}
          />
        </>
      ) : null}
      {hasContent(caption) ? <div className="dw-metric-caption">{caption}</div> : null}
    </div>
  );
}
