// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import { cx } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import "./Tag.css";

export interface TagProps {
  /** Key of a key:value selector, such as "region". */
  label?: string;
  value?: string;
  /** cobalt marks a tag the system manages, such as a pool or a build server. */
  tone?: "cobalt";
  icon?: IconName;
  /** Shows a remove button, for tags that are part of an editable selector. */
  onRemove?: () => void;
  children?: ReactNode;
  className?: string;
}

/** A key:value label or selector, set in mono because machines read it. Tags never carry status. */
export function Tag({ label, value, tone, icon, onRemove, children, className }: TagProps) {
  const strings = useUIStrings();
  const text = [label, value].filter((part) => part != null).join(":");
  return (
    <span className={cx("dw-tag", tone && `dw-tag-${tone}`, className)}>
      {icon ? <Icon name={icon} size={12} /> : null}
      {label != null ? <span className="dw-tag-key">{value != null ? `${label}:` : label}</span> : null}
      {value != null ? <span className="dw-tag-value">{value}</span> : children}
      {onRemove ? (
        <button type="button" className="dw-tag-x" aria-label={strings.remove(text)} onClick={onRemove}>
          <Icon name="x" size={12} />
        </button>
      ) : null}
    </span>
  );
}
