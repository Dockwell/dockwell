// SPDX-License-Identifier: AGPL-3.0-only

import { type InputHTMLAttributes, type ReactNode, type Ref, useId } from "react";

import { cx, hasContent } from "../../lib/cx";
import { TONE } from "../../lib/status";
import { useUIStrings } from "../../lib/strings";
import { Glyph } from "../Glyph/Glyph";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import "./TextField.css";

export interface TextFieldProps extends Omit<InputHTMLAttributes<HTMLInputElement>, "size" | "prefix"> {
  /** Sits above the field, in sentence case. */
  label?: ReactNode;
  /** Says what happens with the value. */
  hint?: ReactNode;
  /** Replaces the hint. Say what is wrong and how to fix it. */
  error?: ReactNode;
  /** Marks it invalid with the message elsewhere, as under a row of fields, which aria-describedby names. */
  invalid?: boolean;
  /** A fixed start of the value, such as "github.com/", so people type only the variable part. */
  prefix?: ReactNode;
  /** A fixed end of the value, such as ".preview.acme.dev". */
  suffix?: ReactNode;
  icon?: IconName;
  /** Mono font for anything a machine parses: paths, hosts, image tags, commands. */
  mono?: boolean;
  /** Marks the uncommon case of a field that may stay empty, instead of an asterisk on every required one. */
  optional?: boolean;
  size?: "sm" | "md" | "lg";
  /** A trailing control rendered inside the field, such as a small ghost Button. */
  action?: ReactNode;
  ref?: Ref<HTMLInputElement>;
}

/** A labelled single-line input with an optional hint, error, prefix, suffix or icon. */
export function TextField({
  label,
  hint,
  error,
  invalid: invalidElsewhere = false,
  prefix,
  suffix,
  icon,
  mono = false,
  optional = false,
  size = "md",
  action,
  className,
  id,
  type = "text",
  "aria-describedby": describedBy,
  "aria-invalid": ariaInvalid,
  ...rest
}: TextFieldProps) {
  const strings = useUIStrings();
  const autoId = useId();
  const inputId = id ?? autoId;
  const messageId = `${inputId}-message`;
  const invalid = hasContent(error);
  const message = invalid ? error : hint;
  const hasMessage = hasContent(message);
  return (
    <div className={cx("dw-field", (invalid || invalidElsewhere) && "is-invalid", className)}>
      {hasContent(label) ? (
        <label className="dw-field-label" htmlFor={inputId}>
          <span>{label}</span>
          {optional ? <span className="dw-field-opt">{strings.optional}</span> : null}
        </label>
      ) : null}
      <div className={cx("dw-input", `dw-input-${size}`, icon && "has-icon")}>
        {icon ? <Icon name={icon} size={16} className="dw-input-icon" /> : null}
        {hasContent(prefix) ? <span className="dw-input-affix dw-input-prefix">{prefix}</span> : null}
        <input
          spellCheck={mono ? false : undefined}
          autoCapitalize={mono ? "off" : undefined}
          {...rest}
          id={inputId}
          type={type}
          className={cx("dw-input-el", mono && "is-mono")}
          aria-invalid={invalid || invalidElsewhere ? true : ariaInvalid}
          aria-describedby={cx(hasMessage && messageId, describedBy) || undefined}
        />
        {hasContent(suffix) ? <span className="dw-input-affix dw-input-suffix">{suffix}</span> : null}
        {action}
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
