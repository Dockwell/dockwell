// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { FileService, FileType } from "@dockwell/api";
import type { FolderListing } from "@dockwell/ui";
import { useCallback, useMemo } from "react";

import { useMay } from "../auth/may";

/**
 * What a folder picker reads of a server's folders: each folder's own, through
 * FileService, as a lookup, so a guess at a path goes on no Activity. Undefined
 * where its reader may not list a server's files, which is an admin's (ADR 0048),
 * or while there's no server to read: the path is typed then.
 */
export function useServerFolders(server: string | undefined): ((path: string) => Promise<FolderListing>) | undefined {
  const transport = useTransport();
  const client = useMemo(() => createClient(FileService, transport), [transport]);
  const allowed = useMay()(FileService.method.listFiles).allowed;
  const list = useCallback(
    async (path: string): Promise<FolderListing> => {
      const read = await client.listFiles({ serverId: server ?? "", path, lookup: true });
      return {
        path: read.path,
        folders: read.entries
          .filter(
            (e) => e.type === FileType.DIRECTORY || (e.type === FileType.SYMLINK && e.linkType === FileType.DIRECTORY),
          )
          .map((e) => ({ name: e.name, locked: e.refused })),
      };
    },
    [client, server],
  );
  return allowed && server ? list : undefined;
}
