// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import { cx } from "../../lib/cx";
import { STATUS, type Status } from "../../lib/status";
import { useUIStrings } from "../../lib/strings";
import { Glyph } from "../Glyph/Glyph";
import "./StatusBadge.css";

export interface StatusBadgeProps {
  status: Status;
  /** Defaults to the status word ("Healthy"). Pass a server or app name to label the subject instead. */
  label?: ReactNode;
  /** Muted trailing text, such as "for 6 min". */
  meta?: ReactNode;
  /** dot in tables, lists and timelines; pill in headers and cards where the status is the headline. */
  variant?: "dot" | "pill";
  title?: string;
  className?: string;
}

/**
 * The state of a server, app, deploy or job as a shape, a word and a colour
 * together, so it reads without colour and in grayscale.
 */
export function StatusBadge({ status, label, meta, variant = "dot", title, className }: StatusBadgeProps) {
  const spec = STATUS[status];
  const word = useUIStrings().status[status];
  const shown = label === undefined ? word : label;
  // When the label names the subject, screen readers still hear the state.
  const announceWord = shown !== word;
  return (
    <span className={cx("dw-status", `dw-status-${variant}`, `dw-tone-${spec.tone}`, className)} title={title}>
      <Glyph shape={spec.shape} size={variant === "pill" ? 8 : 10} />
      {announceWord ? <span className="dw-sr">{word}: </span> : null}
      {shown ? <span className="dw-status-label">{shown}</span> : null}
      {meta ? <span className="dw-status-meta">{meta}</span> : null}
    </span>
  );
}
