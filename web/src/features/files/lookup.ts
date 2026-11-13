// SPDX-License-Identifier: AGPL-3.0-only

import type { ConnectError } from "@connectrpc/connect";
import { useQuery } from "@connectrpc/connect-query";
import { type FileEntry, FileService, type ListFilesResponse } from "@dockwell/api";

import { reasonOf } from "../../lib/errors";
import { nameOf, parentOf } from "./paths";

/** What a form knows of a folder a person chose on a server, as it reads it. */
export interface FolderLookup {
  /** It's asked of the server: there's one, and the path is absolute, with no . or .. in it. */
  asked: boolean;
  /** Still reading. */
  pending: boolean;
  /**
   * What's at the path: a folder, with what it holds; something that isn't a folder;
   * nothing yet; or what couldn't be read of it, with why. Undefined while it's not
   * asked or still reading, and where only the folder above couldn't be read.
   */
  state?: "folder" | "not-a-folder" | "missing" | "unread";
  /** What the folder holds, for a folder. */
  listing?: ListFilesResponse;
  /** The folder above, for a path that isn't there: it's made as that one is owned. */
  parent?: FileEntry;
  /** Neither the path nor the folder above it is there. */
  parentMissing: boolean;
  /** What stopped it reading, for unread. */
  error?: ConnectError;
}

/** A path a form asks the server about: absolute, past the top, with no . or .. in it. */
export function askable(server: string, path: string): boolean {
  return server !== "" && path.startsWith("/") && path.length > 1 && !/\/\.\.?(\/|$)/.test(path);
}

/**
 * Looks a chosen folder up on the server by its own path, as a lookup that goes on no
 * Activity: a folder's listing is the folder, "not a directory" is something else
 * there, and "not found" is nothing yet. Only then does it read the folder above, for
 * its owner, by the name alone, so a folder above of any size answers at once (#588).
 * A folder above that holds more than a listing carries never hides the one asked for.
 */
export function useFolderLookup(server: string, path: string): FolderLookup {
  const asked = askable(server, path);
  const options = { enabled: asked, retry: false, refetchOnWindowFocus: false } as const;
  const here = useQuery(FileService.method.listFiles, { serverId: server, path, lookup: true }, options);
  const reason = here.error ? reasonOf(here.error) : "";
  const gone = reason === "not_found";
  const above = useQuery(
    FileService.method.listFiles,
    { serverId: server, path: parentOf(path), lookup: true, match: nameOf(path) },
    { ...options, enabled: asked && gone },
  );
  if (!asked) return { asked, pending: false, parentMissing: false };
  if (here.isPending) return { asked, pending: true, parentMissing: false };
  if (here.data) return { asked, pending: false, state: "folder", listing: here.data, parentMissing: false };
  if (reason === "not_a_directory") return { asked, pending: false, state: "not-a-folder", parentMissing: false };
  if (!gone)
    return {
      asked,
      pending: false,
      state: "unread",
      parentMissing: false,
      ...(here.error ? { error: here.error } : {}),
    };
  if (above.isPending) return { asked, pending: true, parentMissing: false };
  if (above.error) {
    // The folder above not being there either means the same, as both are made. What else
    // can't be read of it says nothing of this one, so the form says nothing.
    const why = reasonOf(above.error);
    if (why === "not_found") return { asked, pending: false, state: "missing", parentMissing: true };
    // An agent older than the control plane can't look for a name: it's still not there.
    if (why === "agent_outdated") return { asked, pending: false, state: "missing", parentMissing: false };
    return { asked, pending: false, parentMissing: false };
  }
  // A link that leads nowhere lists as not found, and is there all the same.
  if (above.data.entries.some((e) => e.name === nameOf(path))) {
    return { asked, pending: false, state: "not-a-folder", parentMissing: false };
  }
  return {
    asked,
    pending: false,
    state: "missing",
    parentMissing: false,
    ...(above.data.directory ? { parent: above.data.directory } : {}),
  };
}
