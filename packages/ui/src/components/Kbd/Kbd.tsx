// SPDX-License-Identifier: AGPL-3.0-only

import type { HTMLAttributes, ReactNode } from "react";

import { cx } from "../../lib/cx";
import "./Kbd.css";

export interface KbdProps extends HTMLAttributes<HTMLElement> {
  /** One key per cap: ⌘ and K are two caps. Symbols on macOS (⌘ ⌥ ⇧ ↵), words elsewhere (Ctrl, Enter). */
  children: ReactNode;
}

/** A key cap that teaches a shortcut where the action lives. */
export function Kbd({ children, className, ...rest }: KbdProps) {
  return (
    <kbd {...rest} className={cx("dw-kbd", className)}>
      {children}
    </kbd>
  );
}
