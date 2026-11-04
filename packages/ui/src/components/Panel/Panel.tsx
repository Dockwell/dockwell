// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, useId } from "react";

import { cx, hasContent } from "../../lib/cx";
import "./Panel.css";

export interface PanelProps {
  /** The panel's heading, in the heading text style. */
  title?: ReactNode;
  /** Controls on the right of the heading row, such as a StatusBadge or small buttons. */
  actions?: ReactNode;
  /** Drops the body padding, for tables and lists that run edge to edge. */
  flush?: boolean;
  children?: ReactNode;
  className?: string;
}

/**
 * A resting surface for a working area: a line border and no shadow. Panels
 * sit space-6 apart on the canvas. Named by its title for assistive technology.
 */
export function Panel({ title, actions, flush = false, children, className }: PanelProps) {
  const titleId = useId();
  const hasTitle = hasContent(title);
  const hasActions = hasContent(actions);
  return (
    <section className={cx("dw-panel", className)} aria-labelledby={hasTitle ? titleId : undefined}>
      {hasTitle || hasActions ? (
        <div className="dw-panel-head">
          {hasTitle ? (
            <h2 className="dw-panel-title" id={titleId}>
              {title}
            </h2>
          ) : (
            <span />
          )}
          {hasActions ? <div className="dw-panel-actions">{actions}</div> : null}
        </div>
      ) : null}
      <div className={cx("dw-panel-body", flush && "is-flush")}>{children}</div>
    </section>
  );
}
