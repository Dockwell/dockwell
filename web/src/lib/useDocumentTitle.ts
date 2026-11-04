// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect } from "react";

/** Names the browser tab for the page on show: "Servers · Dockwell". Every page sets one, so none keeps the last page's. */
export function useDocumentTitle(title: string): void {
  useEffect(() => {
    document.title = `${title} · Dockwell`;
  }, [title]);
}
