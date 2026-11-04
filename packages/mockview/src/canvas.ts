// SPDX-License-Identifier: AGPL-3.0-only

// Reads design/mocks/canvas.json: every artboard in canvas order, with its
// size and canvas page. It's plain TypeScript with no imports, so the viewer
// and the snapshot script, which Node runs as it is, share one reading.

export interface Board {
  file: string;
  title: string;
  /** The canvas page (area) the artboard sits on. */
  page: string;
  width: number;
  height: number;
}

export interface Page {
  id: string;
  name: string;
  boards: Board[];
}

/** design/mocks/canvas.json, as far as the viewer reads it. */
export interface Canvas {
  boards: Record<string, { title?: string; page?: string; w?: number; h?: number }>;
  order?: string[];
  pages?: { id: string; name: string }[];
}

/** The size of an artboard that canvas.json gives no size: a desktop screen. */
const DEFAULT_WIDTH = 1440;
const DEFAULT_HEIGHT = 900;

/** Every artboard in canvas order, then any the order leaves out, by name. Each missing field gets its default. */
export function canvasBoards(canvas: Canvas): Board[] {
  const files = Object.keys(canvas.boards);
  const order = [...new Set(canvas.order ?? [])].filter((f) => Object.hasOwn(canvas.boards, f));
  const rest = files.filter((f) => !order.includes(f)).sort();
  return [...order, ...rest].map((file) => {
    const b = canvas.boards[file] ?? {};
    return {
      file,
      title: b.title ?? file.replace(/\.dc\.html$/, ""),
      page: b.page ?? "",
      width: b.w ?? DEFAULT_WIDTH,
      height: b.h ?? DEFAULT_HEIGHT,
    };
  });
}

/** Canvas pages in order, each with its artboards, then any page only an artboard names. Empty pages are left out. */
export function canvasPages(canvas: Canvas, boards: readonly Board[]): Page[] {
  const known = canvas.pages ?? [];
  const ids = new Set([...known.map((p) => p.id), ...boards.map((b) => b.page)]);
  const out: Page[] = [];
  for (const id of ids) {
    const list = boards.filter((b) => b.page === id);
    if (list.length) out.push({ id, name: known.find((p) => p.id === id)?.name ?? id, boards: list });
  }
  return out;
}
