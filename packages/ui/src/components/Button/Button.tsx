// SPDX-License-Identifier: AGPL-3.0-only

import type { AnchorHTMLAttributes, ButtonHTMLAttributes, Ref } from "react";

import { cx, hasContent } from "../../lib/cx";
import { keyShortcut } from "../../lib/keyShortcut";
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
  /**
   * Shortcut hint shown inside the button, such as "⌘↵". Assistive technology
   * gets it as aria-keyshortcuts ("Meta+Enter") rather than in the name. Pass
   * aria-keyshortcuts to say it another way, such as "Control+Enter" off macOS.
   */
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
  const iconOnly = !hasContent(children);
  return (
    <button
      {...rest}
      aria-keyshortcuts={rest["aria-keyshortcuts"] ?? (kbd ? keyShortcut(kbd) : undefined)}
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
      {/* Kept out of the name, so the button reads "Apply plan", not "Apply plan⌘↵". */}
      {kbd ? <Kbd aria-hidden="true">{kbd}</Kbd> : null}
    </button>
  );
}

export interface ButtonLinkProps extends AnchorHTMLAttributes<HTMLAnchorElement> {
  /** As for Button. */
  variant?: ButtonProps["variant"];
  size?: ButtonProps["size"];
  /** Icon before the label. With no children the link becomes square and needs aria-label. */
  icon?: IconName;
  iconAfter?: IconName;
  ref?: Ref<HTMLAnchorElement>;
}

/**
 * Goes somewhere and looks like a Button: another page, a download. It is an
 * <a>, so it opens in a new tab and downloads as any link does. Use Button
 * for actions. Inside the panel, wrap it with the router's link factory,
 * such as TanStack Router's createLink, so it navigates without a reload.
 */
export function ButtonLink({
  variant = "secondary",
  size = "md",
  icon,
  iconAfter,
  className,
  children,
  ...rest
}: ButtonLinkProps) {
  const iconOnly = !hasContent(children);
  return (
    <a {...rest} className={cx("dw-btn", `dw-btn-${variant}`, `dw-btn-${size}`, iconOnly && "dw-btn-icon", className)}>
      {icon ? <Icon name={icon} size={size === "sm" ? 14 : 16} /> : null}
      {iconOnly ? null : <span className="dw-btn-label">{children}</span>}
      {iconAfter ? <Icon name={iconAfter} size={14} /> : null}
    </a>
  );
}
