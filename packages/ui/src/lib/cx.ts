// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

/** Joins the truthy class names. */
export function cx(...names: (string | false | null | undefined)[]): string {
  return names.filter(Boolean).join(" ");
}

/**
 * Whether React would render something for node: not null, undefined, a
 * boolean or an empty string. Optional slots use it to leave out their
 * wrapper, and the ids that point at it, when there's nothing to show.
 */
export function hasContent(node: ReactNode): boolean {
  return node != null && typeof node !== "boolean" && node !== "";
}
