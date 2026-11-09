// SPDX-License-Identifier: AGPL-3.0-only

/*
 * Uploads and downloads (docs/adr/0045-a-file-manager-on-a-server.md). An upload is
 * BeginUpload, then its bytes in parts, each from where the last one ended, then
 * FinishUpload; a download is a link the browser saves as it comes. Both reach the
 * API through @dockwell/api's uploadFilePart and downloadFileURL, beside the
 * generated client, as a browser can't stream either over Connect. What's here is
 * what the panel decides: how large a part is, and what an archive is.
 */

/**
 * How much of a file one PUT carries. The control plane passes it on a frame at a
 * time, so any size would do, but a proxy in front of it may take 100 MB at most, as
 * Cloudflare's does. What arrived of a part that drops is kept, so it costs a frame.
 */
export const PART_BYTES = 64 << 20;

/** The most an upload may be, as the agent takes it: 16 GB, in the panel's steps of 1024. */
export const MAX_UPLOAD_BYTES = 16 * 2 ** 30;

/** Archives an upload can unpack, by how their names end. */
const ARCHIVES: readonly [string, "zip" | "tarGz"][] = [
  [".zip", "zip"],
  [".tar.gz", "tarGz"],
  [".tgz", "tarGz"],
];

/** What kind of archive a file is by its name, if it's one an upload can unpack. */
export function archiveKind(name: string): "zip" | "tarGz" | undefined {
  const lower = name.toLowerCase();
  for (const [ext, kind] of ARCHIVES) {
    if (lower.endsWith(ext) && lower.length > ext.length) return kind;
  }
  return undefined;
}

/** The folder an archive unpacks into, beside it, as the agent names it. */
export function unpackedName(name: string): string {
  const lower = name.toLowerCase();
  for (const ext of [".tar.gz", ".tgz", ".zip"]) {
    if (lower.endsWith(ext) && lower.length > ext.length) return name.slice(0, name.length - ext.length);
  }
  return `${name}-unpacked`;
}
