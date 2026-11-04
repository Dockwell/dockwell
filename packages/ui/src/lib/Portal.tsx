// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";
import { createPortal } from "react-dom";

/** Renders overlays at the end of the body, above the page and outside its stacking contexts. */
export function Portal({ children }: { children: ReactNode }) {
  if (typeof document === "undefined") return null;
  return createPortal(children, document.body);
}
