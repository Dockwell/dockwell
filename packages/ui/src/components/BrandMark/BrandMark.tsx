// SPDX-License-Identifier: AGPL-3.0-only

import { cx } from "../../lib/cx";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import { BRANDS, type BrandId } from "./brands";
import "./BrandMark.css";

export interface BrandMarkProps {
  /** The service whose own mark it is. Left out, the tile draws `icon` from the product's set. */
  brand?: BrandId | undefined;
  /** The product's own icon, for a service with no mark of its own, such as another S3-compatible store. */
  icon?: IconName;
  /** The tile's side in pixels. Default 32. */
  size?: number;
  /**
   * What a screen reader says for it. Left out, the tile is decorative, as it is beside
   * the service's name, which is the usual place for it.
   */
  label?: string;
  className?: string;
}

/**
 * A service's own mark on a tile, as the catalog's app icons are drawn: on the light art
 * ground in both themes, on the dark one where the mark is mostly light, or edge to edge
 * where it brings its own square. Drawn inline, with no request to another host.
 */
export function BrandMark({ brand, icon = "link", size = 32, label, className }: BrandMarkProps) {
  const b = brand ? BRANDS[brand] : undefined;
  const a11y = label ? { role: "img", "aria-label": label } : { "aria-hidden": true as const };
  return (
    <span
      className={cx("dw-brand-mark", className)}
      data-ground={b ? b.ground : "none"}
      style={{ width: size, height: size }}
      {...a11y}
    >
      {b ? (
        <svg viewBox={b.viewBox} focusable="false" aria-hidden="true">
          {b.art}
        </svg>
      ) : (
        <Icon name={icon} size={Math.round(size / 2)} />
      )}
    </span>
  );
}
