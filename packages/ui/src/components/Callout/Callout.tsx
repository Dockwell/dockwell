// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import { cx, hasContent } from "../../lib/cx";
import { type GlyphShape, TONE, type Tone } from "../../lib/status";
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
  /** The tone's shape stands for the state unless the state has one of its own, such as the arc of work in flight. */
  shape?: GlyphShape;
  className?: string;
}

/**
 * An inline notice about the current page: what happened, what Dockwell
 * already did, and what the reader can do next. At most one per view.
 */
export function Callout({ tone = "info", title, children, actions, shape, className }: CalloutProps) {
  const spec = TONE[tone];
  return (
    <div
      className={cx("dw-callout", `dw-tone-${spec.tone}`, className)}
      role={tone === "critical" ? "alert" : "status"}
    >
      {/* Not knowing is grey wherever it's drawn, as that shape is, whatever the tone around it. */}
      <span className={cx("dw-callout-glyph", shape === "dashed-ring" && "dw-tone-unknown")}>
        <Glyph shape={shape ?? spec.shape} size={10} />
      </span>
      <div className="dw-callout-body">
        {hasContent(title) ? <div className="dw-callout-title">{title}</div> : null}
        {hasContent(children) ? <div className="dw-callout-text">{children}</div> : null}
      </div>
      {hasContent(actions) ? <div className="dw-callout-actions">{actions}</div> : null}
    </div>
  );
}
