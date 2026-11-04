// SPDX-License-Identifier: AGPL-3.0-only

import { type CSSProperties, type ReactNode, useEffect, useRef, useState } from "react";

import { cx } from "../../lib/cx";
import { TONE, type Tone } from "../../lib/status";
import { useUIStrings } from "../../lib/strings";
import { Button } from "../Button/Button";
import { Glyph } from "../Glyph/Glyph";
import type { IconName } from "../Icon/icons";
import "./Toast.css";

export interface ToastAction {
  /** Usually "Undo". */
  label: string;
  onClick?: () => void;
  icon?: IconName;
  /** Usually "⌘Z". */
  kbd?: string;
}

export interface ToastProps {
  tone?: Tone;
  /** Past tense, matching the button that triggered it: "Rolled back to r127". */
  title: ReactNode;
  description?: ReactNode;
  action?: ToastAction;
  /** Shows a dismiss button, and is called when the duration runs out. The caller removes the toast. */
  onClose?: () => void;
  /**
   * Milliseconds before the toast asks to close, drawn as a timer that pauses
   * while the toast is hovered or focused. Leave it out for failures, which
   * stay until closed.
   */
  duration?: number;
  className?: string;
}

/**
 * Confirms a change that just happened and keeps the way back one keystroke
 * away. A live region: polite, or assertive for critical tones. Never put
 * information only in a toast; the Timeline records everything it announces.
 * Stack toasts in a dw-toaster container, newest at the bottom, at most three.
 */
export function Toast({ tone = "healthy", title, description, action, onClose, duration, className }: ToastProps) {
  const strings = useUIStrings();
  const spec = TONE[tone];
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const paused = hovered || focused;
  const remaining = useRef(duration ?? 0);
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
  });
  const closable = onClose !== undefined;

  useEffect(() => {
    remaining.current = duration ?? 0;
  }, [duration]);

  useEffect(() => {
    if (!duration || !closable || paused) return;
    const started = Date.now();
    const timer = setTimeout(() => onCloseRef.current?.(), Math.max(0, remaining.current));
    return () => {
      clearTimeout(timer);
      remaining.current -= Date.now() - started;
    };
  }, [duration, closable, paused]);

  const style = duration ? ({ "--dw-toast-d": `${duration}ms` } as CSSProperties) : undefined;
  return (
    // Hover and focus pause the timer, as the CSS pauses its bar.
    // biome-ignore lint/a11y/noStaticElementInteractions: the handlers only pause the timer
    <div
      className={cx("dw-toast", `dw-tone-${spec.tone}`, className)}
      role={tone === "critical" ? "alert" : "status"}
      style={style}
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
      onFocus={() => setFocused(true)}
      onBlur={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget)) setFocused(false);
      }}
    >
      <span className="dw-toast-glyph">
        <Glyph shape={spec.shape} size={10} />
      </span>
      <div className="dw-toast-body">
        <div className="dw-toast-title">{title}</div>
        {description != null && description !== false ? <div className="dw-toast-desc">{description}</div> : null}
      </div>
      <div className="dw-toast-actions">
        {action ? (
          <Button size="sm" variant="secondary" icon={action.icon} kbd={action.kbd} onClick={action.onClick}>
            {action.label}
          </Button>
        ) : null}
        {onClose ? <Button size="sm" variant="ghost" icon="x" aria-label={strings.dismiss} onClick={onClose} /> : null}
      </div>
      {duration !== undefined && duration > 0 && closable ? (
        <span className="dw-toast-timer" aria-hidden="true" />
      ) : null}
    </div>
  );
}
