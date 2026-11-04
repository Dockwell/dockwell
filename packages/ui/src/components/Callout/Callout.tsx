// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import { cx } from "../../lib/cx";
import { TONE, type Tone } from "../../lib/status";
import { Glyph } from "../Glyph/Glyph";
import "./Callout.css";

export interface CalloutProps {
  tone?: Tone;
  /** The fact, short: "ams-1-03 is down". */
  title?: ReactNode;
  /** One or two sentences: the consequence, then the fix. */
  children?: ReactNode;
  /** Up to two buttons. */
  actions?: ReactNode;
  className?: string;
}

/**
 * An inline notice about the current page: what happened, what Dockwell
 * already did, and what the reader can do next. At most one per view.
 */
export function Callout({ tone = "info", title, children, actions, className }: CalloutProps) {
  const spec = TONE[tone];
  const has = (node: ReactNode) => node != null && node !== false;
  return (
    <div
      className={cx("dw-callout", `dw-tone-${spec.tone}`, className)}
      role={tone === "critical" ? "alert" : "status"}
    >
      <span className="dw-callout-glyph">
        <Glyph shape={spec.shape} size={10} />
      </span>
      <div className="dw-callout-body">
        {has(title) ? <div className="dw-callout-title">{title}</div> : null}
        {has(children) ? <div className="dw-callout-text">{children}</div> : null}
      </div>
      {has(actions) ? <div className="dw-callout-actions">{actions}</div> : null}
    </div>
  );
}
