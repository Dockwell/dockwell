// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { FilesPage, type FilesSearch } from "../../features/files/FilesPage";

/** Reads where you are on a server from its URL, leaving out what isn't a path. */
function readSearch(search: Record<string, unknown>): FilesSearch {
  const out: FilesSearch = {};
  for (const key of ["path", "file"] as const) {
    const value = search[key];
    if (typeof value === "string" && value.startsWith("/")) out[key] = value;
  }
  return out;
}

/** A server's files: a folder, and the file open beside it. */
export const Route = createFileRoute("/_shell/servers/$serverId/files")({
  validateSearch: readSearch,
  component: function Files() {
    const { serverId } = Route.useParams();
    return <FilesPage serverId={serverId} search={Route.useSearch()} />;
  },
});
