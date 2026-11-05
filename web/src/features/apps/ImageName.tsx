// SPDX-License-Identifier: AGPL-3.0-only

import { cx } from "@dockwell/ui";

/**
 * An image's name that keeps its end when it doesn't fit, as the last part and
 * the tag are what tell two apart: ten rows of the same registry otherwise read
 * as ten copies of it. The start clips first, and hovering says it whole.
 */
export function ImageName({ image, className }: { image: string; className?: string | undefined }) {
  const at = image.lastIndexOf("/") + 1;
  return (
    <code className={cx("image-name", className)} title={image}>
      {at > 0 ? <span className="image-name-path">{image.slice(0, at)}</span> : null}
      <span className="image-name-end">{image.slice(at)}</span>
    </code>
  );
}
