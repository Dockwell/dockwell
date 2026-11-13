// SPDX-License-Identifier: AGPL-3.0-only

import { FileService } from "@dockwell/api";
import type { FolderListing } from "@dockwell/ui";

import { useMay } from "../auth/may";
import { useFolderList } from "../files/folders";

/**
 * What a folder picker reads of a server's folders: each folder's own, through
 * FileService, as a lookup, so a guess at a path goes on no Activity. Undefined
 * where its reader may not list a server's files, which is an admin's (ADR 0048),
 * or while there's no server to read: the path is typed then.
 */
export function useServerFolders(
  server: string | undefined,
): ((path: string, match?: string) => Promise<FolderListing>) | undefined {
  const list = useFolderList(server ?? "");
  const allowed = useMay()(FileService.method.listFiles).allowed;
  return allowed && server ? list : undefined;
}
