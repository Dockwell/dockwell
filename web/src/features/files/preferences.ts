// SPDX-License-Identifier: AGPL-3.0-only

/**
 * Per-viewer file manager preferences stored in localStorage. Every access is
 * guarded against browsers or sandboxes that block or fill storage.
 */

export const DEFAULT_TREE_WIDTH = 220;
export const MIN_TREE_WIDTH = 160;
export const MAX_TREE_WIDTH = 460;

export type ResizableColumn = "name" | "size" | "modified" | "mode" | "owner";
export type ColumnWidths = Partial<Record<ResizableColumn, number>>;

export const IMAGE_PREVIEW_MAX_BYTES = 5 * 1024 * 1024;
export const TEXT_PREVIEW_MAX_BYTES = 1024 * 1024;
export const TEXT_PREVIEW_LINES = 24;

function readStorage(key: string): string | null {
  try {
    return window.localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeStorage(key: string, value: string): void {
  try {
    window.localStorage.setItem(key, value);
  } catch {
    // Storage may be unavailable or full; keep working in memory.
  }
}

export function readTreeWidth(viewerId: string): number {
  const raw = readStorage(`dockwell:files:tree-width:${viewerId || "anon"}`);
  if (!raw) return DEFAULT_TREE_WIDTH;
  const n = Number(raw);
  if (!Number.isFinite(n)) return DEFAULT_TREE_WIDTH;
  return Math.max(MIN_TREE_WIDTH, Math.min(MAX_TREE_WIDTH, Math.round(n)));
}

export function writeTreeWidth(viewerId: string, width: number): void {
  const clamped = Math.max(MIN_TREE_WIDTH, Math.min(MAX_TREE_WIDTH, Math.round(width)));
  writeStorage(`dockwell:files:tree-width:${viewerId || "anon"}`, String(clamped));
}

const VALID_COLUMNS = new Set<ResizableColumn>(["name", "size", "modified", "mode", "owner"]);

export function readColumnWidths(viewerId: string): ColumnWidths {
  const raw = readStorage(`dockwell:files:col-widths:${viewerId || "anon"}`);
  if (!raw) return {};
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    if (!parsed || typeof parsed !== "object") return {};
    const out: ColumnWidths = {};
    for (const [k, v] of Object.entries(parsed)) {
      if (
        VALID_COLUMNS.has(k as ResizableColumn) &&
        typeof v === "number" &&
        Number.isFinite(v) &&
        v >= 56 &&
        v <= 900
      ) {
        out[k as ResizableColumn] = Math.round(v);
      }
    }
    return out;
  } catch {
    return {};
  }
}

export function writeColumnWidths(viewerId: string, widths: ColumnWidths): void {
  writeStorage(`dockwell:files:col-widths:${viewerId || "anon"}`, JSON.stringify(widths));
}

export function readWordWrap(viewerId: string): boolean {
  return readStorage(`dockwell:files:word-wrap:${viewerId || "anon"}`) === "1";
}

export function writeWordWrap(viewerId: string, wrap: boolean): void {
  writeStorage(`dockwell:files:word-wrap:${viewerId || "anon"}`, wrap ? "1" : "0");
}

const IMAGE_MIMES: readonly [string, string][] = [
  [".png", "image/png"],
  [".jpg", "image/jpeg"],
  [".jpeg", "image/jpeg"],
  [".gif", "image/gif"],
  [".webp", "image/webp"],
  [".svg", "image/svg+xml"],
];

/** MIME type for an image file that can be previewed in an <img>, or undefined. */
export function imageMimeOf(name: string): string | undefined {
  const lower = name.toLowerCase();
  for (const [ext, mime] of IMAGE_MIMES) {
    if (lower.endsWith(ext) && lower.length > ext.length) return mime;
  }
  return undefined;
}
