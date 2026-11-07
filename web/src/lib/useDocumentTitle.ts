// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { AuthService } from "@dockwell/api";
import { useEffect } from "react";

/**
 * Names the browser tab for the page on show: "Servers · Dockwell", or with the
 * install's name in place of Dockwell once someone has named it in Settings. Every
 * page sets one, so none keeps the last page's.
 */
export function useDocumentTitle(title: string): void {
  const name = useQuery(AuthService.method.getAuthState, {}, { refetchOnMount: false }).data?.installName;
  useEffect(() => {
    document.title = `${title} · ${name || "Dockwell"}`;
  }, [title, name]);
}
