// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type FileEntry, FileService, FileType, type ListFilesResponse } from "@dockwell/api";
import type { FolderListing } from "@dockwell/ui";
import { useCallback, useMemo } from "react";

/** A folder, or a link that leads to one. */
export function isFolder(e: FileEntry): boolean {
  return e.type === FileType.DIRECTORY || (e.type === FileType.SYMLINK && e.linkType === FileType.DIRECTORY);
}

/**
 * What a folder picker shows of a listing: its folders, by name, and, where the
 * listing was cut at its bound, how many entries it shows of how many there are, so
 * the picker says so and offers to find one by name (#588).
 */
export function folderListing(res: ListFilesResponse): FolderListing {
  return {
    path: res.path,
    folders: res.entries
      .filter((e) => !e.unreadableName && isFolder(e))
      .map((e) => ({ name: e.name, locked: e.refused }))
      .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0)),
    ...(res.truncated ? { cut: { shown: res.entries.length, total: res.total } } : {}),
  };
}

/**
 * Reads a server's folders for a folder picker, through FileService, as lookups: the
 * form reads them, not a person opening a folder, so one the server refuses goes on
 * no Activity. A name to find searches the whole folder on the server, bounded as a
 * listing is.
 */
export function useFolderList(server: string): (path: string, match?: string) => Promise<FolderListing> {
  const transport = useTransport();
  const client = useMemo(() => createClient(FileService, transport), [transport]);
  return useCallback(
    async (path: string, match?: string) =>
      folderListing(await client.listFiles({ serverId: server, path, lookup: true, match: match ?? "" })),
    [client, server],
  );
}
