// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

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
  /** The recent series, drawn as a sparkline. */
  data?: readonly number[];
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
  tone,
  threshold,
  min,
  max,
  caption,
  className,
}: MetricProps) {
  const strings = useUIStrings();
  return (
    <div className={cx("dw-metric", className)}>
      <div className="dw-metric-label">
        {icon ? <Icon name={icon} size={14} /> : null}
        <span>{label}</span>
      </div>
      <div className="dw-metric-row">
        <span className="dw-metric-value">{value}</span>
        {hasContent(unit) ? <span className="dw-metric-unit">{unit}</span> : null}
        {hasContent(delta) ? (
          <span className={cx("dw-metric-delta", deltaTone && `dw-tone-${deltaTone}`)}>{delta}</span>
        ) : null}
      </div>
      {data ? (
        <Sparkline
          data={data}
          tone={tone}
          threshold={threshold}
          width="100%"
          height={36}
          min={min}
          max={max}
          label={typeof label === "string" ? strings.trend(label) : undefined}
        />
      ) : null}
      {hasContent(caption) ? <div className="dw-metric-caption">{caption}</div> : null}
    </div>
  );
}
