// SPDX-License-Identifier: AGPL-3.0-only

/** An image Dockwell built, by its name, as dockwell.invalid/svc_…:<commit>-<change> (ADR 0046). */
const BUILT = /^dockwell\.invalid\/[^:/]+:([0-9a-f]{7,40})-/;

/** The commit an image Dockwell built is of, in 7 digits, or undefined for any other image. */
export function builtCommit(image: string): string | undefined {
  return BUILT.exec(image)?.[1]?.slice(0, 7);
}

/** An image as people read it: the commit an image Dockwell built is of, and any other as it is. */
export function shownImage(image: string): string {
  return builtCommit(image) ?? image;
}

/** A branch or a tag as people name it: main for refs/heads/main, v1.4 for refs/tags/v1.4. */
export function shownRef(ref: string): string {
  return ref.replace(/^refs\/(heads|tags)\//, "");
}
