// SPDX-License-Identifier: AGPL-3.0-only

import {
  type CSSProperties,
  type ReactNode,
  type Ref,
  type TextareaHTMLAttributes,
  useId,
  useMemo,
  useRef,
} from "react";

import { cx, hasContent } from "../../lib/cx";
import { TONE } from "../../lib/status";
import { Glyph } from "../Glyph/Glyph";
import "./CodeField.css";

export interface CodeFieldProps extends Omit<TextareaHTMLAttributes<HTMLTextAreaElement>, "rows" | "value"> {
  /** Sits above the field, in sentence case. */
  label?: ReactNode;
  /** Beside the label, such as a button that reads a file. */
  actions?: ReactNode;
  /** Says what happens with the value. */
  hint?: ReactNode;
  /** Replaces the hint. Say what is wrong and how to fix it. */
  error?: ReactNode;
  /** The line the error is about, from 1, which the field marks. */
  markedLine?: number | undefined;
  /** How many lines show before it scrolls. */
  lines?: number;
  value: string;
  ref?: Ref<HTMLTextAreaElement>;
}

/**
 * A multi-line input for text a machine parses, such as a Compose file, with
 * line numbers, so an error can point at a line. Long lines scroll sideways
 * rather than wrap, so a line number always means one line.
 */
export function CodeField({
  label,
  actions,
  hint,
  error,
  markedLine,
  lines = 12,
  value,
  className,
  id,
  onScroll,
  "aria-describedby": describedBy,
  "aria-invalid": ariaInvalid,
  ...rest
}: CodeFieldProps) {
  const autoId = useId();
  const inputId = id ?? autoId;
  const messageId = `${inputId}-message`;
  const invalid = hasContent(error);
  const message = invalid ? error : hint;
  const hasMessage = hasContent(message);
  const gutter = useRef<HTMLDivElement>(null);
  const marks = useRef<HTMLDivElement>(null);
  const count = useMemo(() => Math.max(value.split("\n").length, 1), [value]);
  const numbers = useMemo(() => Array.from({ length: count }, (_, i) => i + 1).join("\n"), [count]);
  return (
    <div className={cx("dw-field", "dw-code-field", invalid && "is-invalid", className)}>
      {hasContent(label) || hasContent(actions) ? (
        <div className="dw-code-head">
          {hasContent(label) ? (
            <label className="dw-field-label" htmlFor={inputId}>
              {label}
            </label>
          ) : (
            <span />
          )}
          {hasContent(actions) ? <div className="dw-code-actions">{actions}</div> : null}
        </div>
      ) : null}
      <div className="dw-code-input" style={{ "--dw-code-lines": lines } as CSSProperties}>
        <div className="dw-code-gutter" aria-hidden="true">
          <div ref={gutter} className="dw-code-numbers">
            {numbers}
          </div>
        </div>
        <div className="dw-code-body">
          <div ref={marks} className="dw-code-marks" aria-hidden="true">
            {markedLine && markedLine > 0 ? (
              <div className="dw-code-mark" style={{ "--dw-code-line": markedLine - 1 } as CSSProperties} />
            ) : null}
          </div>
          <textarea
            spellCheck={false}
            autoCapitalize="off"
            autoCorrect="off"
            {...rest}
            id={inputId}
            value={value}
            wrap="off"
            className="dw-code-el"
            aria-invalid={invalid ? true : ariaInvalid}
            aria-describedby={cx(hasMessage && messageId, describedBy) || undefined}
            onScroll={(event) => {
              const top = event.currentTarget.scrollTop;
              if (gutter.current) gutter.current.style.transform = `translateY(${-top}px)`;
              if (marks.current) marks.current.style.transform = `translateY(${-top}px)`;
              onScroll?.(event);
            }}
          />
        </div>
      </div>
      {hasMessage ? (
        <div id={messageId} className={invalid ? "dw-field-error" : "dw-field-hint"}>
          {invalid ? <Glyph shape={TONE.critical.shape} size={9} /> : null}
          <span>{message}</span>
        </div>
      ) : null}
    </div>
  );
}
