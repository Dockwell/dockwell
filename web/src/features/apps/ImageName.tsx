// SPDX-License-Identifier: AGPL-3.0-only

import { cx } from "@dockwell/ui";
import { Fragment } from "react";

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

/**
 * An image's name in full, for where it has room to wrap: after a slash, where
 * it can, rather than in the middle of a word. Copying it takes it as it is.
 */
export function WholeImage({ image }: { image: string }) {
  const parts = image.split("/");
  return (
    <code>
      {parts.map((part, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: the parts of one name, which never move
        <Fragment key={i}>
          {part}
          {i < parts.length - 1 ? (
            <>
              /<wbr />
            </>
          ) : null}
        </Fragment>
      ))}
    </code>
  );
}
