// SPDX-License-Identifier: AGPL-3.0-only

import type { ButtonHTMLAttributes, Ref } from "react";

import { cx } from "../../lib/cx";
import { Glyph } from "../Glyph/Glyph";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import { Kbd } from "../Kbd/Kbd";
import "./Button.css";

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  /** primary is ink, one per view. secondary is the default. danger only inside a confirmation or a plan. */
  variant?: "primary" | "secondary" | "ghost" | "danger";
  size?: "sm" | "md" | "lg";
  /** Icon before the label. With no children the button becomes square and needs aria-label. */
  icon?: IconName;
  iconAfter?: IconName;
  /** Shortcut hint shown inside the button, such as "⌘↵". */
  kbd?: string;
  /** Replaces the icon with a spinner and disables the button. Keep the label and change its verb ("Deploying"). */
  loading?: boolean;
  ref?: Ref<HTMLButtonElement>;
}

/** Runs an action. The label is a verb that names the result: "Apply plan", "Restart", "Remove server". */
export function Button({
  variant = "secondary",
  size = "md",
  icon,
  iconAfter,
  kbd,
  loading = false,
  className,
  children,
  type = "button",
  disabled,
  ...rest
}: ButtonProps) {
  const iconOnly = children == null || children === false;
  return (
    <button
      {...rest}
      type={type}
      className={cx(
        "dw-btn",
        `dw-btn-${variant}`,
        `dw-btn-${size}`,
        iconOnly && "dw-btn-icon",
        loading && "is-loading",
        className,
      )}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
    >
      {loading ? (
        <Glyph shape="spinner" size={size === "sm" ? 12 : 14} className="dw-btn-spinner" />
      ) : icon ? (
        <Icon name={icon} size={size === "sm" ? 14 : 16} />
      ) : null}
      {iconOnly ? null : <span className="dw-btn-label">{children}</span>}
      {iconAfter ? <Icon name={iconAfter} size={14} /> : null}
      {kbd ? <Kbd>{kbd}</Kbd> : null}
    </button>
  );
}
