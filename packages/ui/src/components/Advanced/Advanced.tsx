// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, useId } from "react";

import { cx, hasContent } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import { useControlled } from "../../lib/useControlled";
import { Icon } from "../Icon/Icon";
import "./Advanced.css";

export interface AdvancedProps {
  /** What it holds, in sentence case. Defaults to "Advanced". */
  label?: ReactNode;
  /**
   * The defaults inside, in one line, shown while it's closed, such as
   * "Restarts unless stopped · no limits". It is what a person reads instead of
   * opening it, so it names values, not what the section is for.
   */
  summary?: ReactNode;
  /** Stays beside the label, open or closed, such as a tag that says how many settings were changed. */
  badge?: ReactNode;
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  className?: string;
  children?: ReactNode;
}

/**
 * The settings a form starts at good defaults, out of the way until someone
 * wants them (defaults first, power one step away). Closed, it says what the
 * defaults are in one line, so nobody opens it to find out. A form that refuses
 * a field inside opens it, as it controls open.
 */
export function Advanced({
  label,
  summary,
  badge,
  open: openProp,
  defaultOpen = false,
  onOpenChange,
  className,
  children,
}: AdvancedProps) {
  const strings = useUIStrings();
  const [open, setOpen] = useControlled(openProp, defaultOpen, onOpenChange);
  const bodyId = useId();
  const labelId = useId();
  const summaryId = useId();
  const showsSummary = !open && hasContent(summary);
  return (
    <section className={cx("dw-advanced", open && "is-open", className)}>
      <button
        type="button"
        className="dw-advanced-toggle"
        aria-expanded={open}
        aria-controls={open ? bodyId : undefined}
        aria-labelledby={labelId}
        aria-describedby={showsSummary ? summaryId : undefined}
        onClick={() => setOpen(!open)}
      >
        <Icon name={open ? "chevron-down" : "chevron-right"} size={14} className="dw-advanced-chevron" />
        <span id={labelId} className="dw-advanced-label">
          {hasContent(label) ? label : strings.advanced}
        </span>
        {hasContent(badge) ? <span className="dw-advanced-badge">{badge}</span> : null}
        {showsSummary ? (
          <span id={summaryId} className="dw-advanced-summary">
            {summary}
          </span>
        ) : null}
      </button>
      {open ? (
        <div id={bodyId} className="dw-advanced-body">
          {children}
        </div>
      ) : null}
    </section>
  );
}
