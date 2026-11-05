// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, type Ref, type SelectHTMLAttributes, useId } from "react";

import { cx, hasContent } from "../../lib/cx";
import { TONE } from "../../lib/status";
import { Glyph } from "../Glyph/Glyph";
import { Icon } from "../Icon/Icon";
import "../TextField/TextField.css";
import "./SelectField.css";

export interface SelectOption {
  value: string;
  label: string;
}

export interface SelectFieldProps extends Omit<SelectHTMLAttributes<HTMLSelectElement>, "size"> {
  /** Sits above the field, in sentence case. */
  label?: ReactNode;
  /** Says what happens with the value. */
  hint?: ReactNode;
  /** Replaces the hint. Say what is wrong and how to fix it. */
  error?: ReactNode;
  options: readonly SelectOption[];
  /** Mono font for options a machine parses, such as server names. */
  mono?: boolean;
  size?: "sm" | "md" | "lg";
  ref?: Ref<HTMLSelectElement>;
}

/**
 * One choice among more than a SegmentedControl holds, or of different kinds.
 * A native select, so it behaves as the platform's does on every phone. Never
 * show one with a single option.
 */
export function SelectField({
  label,
  hint,
  error,
  options,
  mono = false,
  size = "md",
  className,
  id,
  "aria-describedby": describedBy,
  ...rest
}: SelectFieldProps) {
  const autoId = useId();
  const inputId = id ?? autoId;
  const messageId = `${inputId}-message`;
  const invalid = hasContent(error);
  const message = invalid ? error : hint;
  const hasMessage = hasContent(message);
  return (
    <div className={cx("dw-field", invalid && "is-invalid", className)}>
      {hasContent(label) ? (
        <label className="dw-field-label" htmlFor={inputId}>
          {label}
        </label>
      ) : null}
      <div className={cx("dw-input", `dw-input-${size}`, "dw-select")}>
        <select
          {...rest}
          id={inputId}
          className={cx("dw-input-el", "dw-select-el", mono && "is-mono")}
          aria-invalid={invalid || undefined}
          aria-describedby={cx(hasMessage && messageId, describedBy) || undefined}
        >
          {options.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
        <Icon name="chevron-up-down" size={16} className="dw-select-icon" />
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
