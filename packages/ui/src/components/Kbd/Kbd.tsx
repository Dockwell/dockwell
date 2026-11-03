// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import { cx } from "../../lib/cx";
import "./Kbd.css";

export interface KbdProps {
  /** One key per cap: ⌘ and K are two caps. Symbols on macOS (⌘ ⌥ ⇧ ↵), words elsewhere (Ctrl, Enter). */
  children: ReactNode;
  className?: string;
}

/** A key cap that teaches a shortcut where the action lives. */
export function Kbd({ children, className }: KbdProps) {
  return <kbd className={cx("dw-kbd", className)}>{children}</kbd>;
}
