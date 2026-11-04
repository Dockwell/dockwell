// SPDX-License-Identifier: AGPL-3.0-only

// URLs and messages shared by the viewer and the frame that renders one artboard.

/** An explicit theme, or null for the theme each artboard is drawn in. */
export type ThemeChoice = "light" | "dark" | null;

/** The message a frame sends its viewer when a link inside the mock points at another artboard. */
export const OPEN_MESSAGE = "dockwell:mockview-open";

export interface OpenMessage {
  type: typeof OPEN_MESSAGE;
  file: string;
  hash: string;
}

export function isOpenMessage(data: unknown): data is OpenMessage {
  if (typeof data !== "object" || data === null) return false;
  const d = data as Record<string, unknown>;
  return d["type"] === OPEN_MESSAGE && typeof d["file"] === "string" && typeof d["hash"] === "string";
}

/** What the viewer's keys do: the arrow keys step through the canvas order, and Escape goes back to the list. */
export type Shortcut = "previous" | "next" | "back";

/**
 * The message a frame sends its viewer for a shortcut key pressed inside the
 * mock. Focus in the frame keeps key presses from reaching the viewer's window.
 */
export const KEY_MESSAGE = "dockwell:mockview-key";

export interface KeyMessage {
  type: typeof KEY_MESSAGE;
  shortcut: Shortcut;
}

const KEYS = new Map<string, Shortcut>([
  ["ArrowLeft", "previous"],
  ["ArrowRight", "next"],
  ["Escape", "back"],
]);

export function isKeyMessage(data: unknown): data is KeyMessage {
  if (typeof data !== "object" || data === null) return false;
  const d = data as Record<string, unknown>;
  return d["type"] === KEY_MESSAGE && [...KEYS.values()].includes(d["shortcut"] as Shortcut);
}

/** True when a key press goes to something the person types in, which keeps its arrows and Escape. */
function isTyping(target: EventTarget | null): boolean {
  if (!(target instanceof Element)) return false;
  if (target.closest("input, textarea, select")) return true;
  // jsdom has no isContentEditable, so the attribute stands in for it there.
  if (target instanceof HTMLElement && typeof target.isContentEditable === "boolean") return target.isContentEditable;
  return target.closest('[contenteditable]:not([contenteditable="false"])') !== null;
}

/**
 * The viewer shortcut a key press stands for, or null. Typing, a key with a
 * modifier, and a key the mock already handled, such as the arrows in a
 * segmented control, are left alone.
 */
export function shortcutFor(event: KeyboardEvent): Shortcut | null {
  if (event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey) return null;
  const shortcut = KEYS.get(event.key);
  return shortcut && !isTyping(event.target) ? shortcut : null;
}

export function parseTheme(value: string | null | undefined): ThemeChoice {
  return value === "light" || value === "dark" ? value : null;
}

const ARTBOARD_LINK = /^(?:\.\/|\/)?([\w.-]+\.dc\.html)(#[^#]*)?$/;

/** The artboard an href inside a mock points at, such as "Alerts.dc.html#rules", or null for any other link. */
export function artboardLink(href: string): { file: string; hash: string } | null {
  const m = ARTBOARD_LINK.exec(href.trim());
  return m?.[1] ? { file: m[1], hash: m[2] ?? "" } : null;
}

/** The page that renders one artboard on its own, relative to the viewer. */
export function frameUrl(file: string, theme: ThemeChoice, hash = ""): string {
  const params = new URLSearchParams({ board: file });
  if (theme) params.set("theme", theme);
  return `frame.html?${params}${hash}`;
}

export interface ViewerRoute {
  /** The artboard on show, or null for the list of every artboard. */
  file: string | null;
  theme: ThemeChoice;
  /** A fragment inside the artboard to scroll to, such as "#rules". */
  hash: string;
}

/** Viewer locations live in the URL fragment: #/Overview-Single.dc.html?theme=dark. */
export function viewerHash({ file, theme, hash }: ViewerRoute): string {
  const params = new URLSearchParams();
  if (theme) params.set("theme", theme);
  if (hash) params.set("at", hash.replace(/^#/, ""));
  const query = params.size ? `?${params}` : "";
  return `#/${file ? encodeURIComponent(file) : ""}${query}`;
}

/** Percent-decodes a URL part, or returns it as written when it isn't valid percent-encoding, such as "50%off". */
export function decodePart(text: string): string {
  try {
    return decodeURIComponent(text);
  } catch {
    return text;
  }
}

export function parseViewerHash(hash: string): ViewerRoute {
  const raw = hash.replace(/^#\/?/, "");
  const q = raw.indexOf("?");
  const path = decodePart(q === -1 ? raw : raw.slice(0, q));
  const params = new URLSearchParams(q === -1 ? "" : raw.slice(q + 1));
  const at = params.get("at");
  return { file: path || null, theme: parseTheme(params.get("theme")), hash: at ? `#${at}` : "" };
}
