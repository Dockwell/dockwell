// SPDX-License-Identifier: AGPL-3.0-only

import { cx } from "../../lib/cx";
import { type IconName, type IconPart, icons } from "./icons";
import "./Icon.css";

export interface IconProps {
  name: IconName;
  /** Pixels: 16 in controls and lists, 14 in small buttons and tags, 20 in empty states. */
  size?: number;
  /** Accessible name, when the icon stands alone without a visible label. Otherwise it is hidden. */
  label?: string;
  strokeWidth?: number;
  className?: string;
}

/** A line icon drawn in currentColor. Status is never an icon: use StatusBadge for state. */
export function Icon({ name, size = 16, label, strokeWidth = 1.5, className }: IconProps) {
  return (
    <svg
      className={cx("dw-icon", className)}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={strokeWidth}
      strokeLinecap="round"
      strokeLinejoin="round"
      focusable="false"
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
    >
      {icons[name].map((part, i) => (
        // Icon parts are static and never reorder, so the index is a stable key.
        // biome-ignore lint/suspicious/noArrayIndexKey: see above
        <IconElement key={i} part={part} />
      ))}
    </svg>
  );
}

function IconElement({ part }: { part: IconPart }) {
  switch (part[0]) {
    case "path":
      return <path {...part[1]} />;
    case "circle":
      return <circle {...part[1]} />;
    case "ellipse":
      return <ellipse {...part[1]} />;
    case "rect":
      return <rect {...part[1]} />;
  }
}
