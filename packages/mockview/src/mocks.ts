// SPDX-License-Identifier: AGPL-3.0-only

// The mocks in design/mocks, read where they are: nothing is copied.

import canvasJson from "../../../design/mocks/canvas.json";

const sources = import.meta.glob<string>("../../../design/mocks/*.dc.html", { query: "?raw", import: "default" });
const PREFIX = "../../../design/mocks/";

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

interface CanvasBoard {
  title?: string;
  page?: string;
  w?: number;
  h?: number;
}

const canvas = canvasJson as unknown as {
  boards: Record<string, CanvasBoard>;
  order?: string[];
  pages?: { id: string; name: string }[];
};

/** Every artboard in canvas order, then any the order leaves out. */
export const boards: readonly Board[] = (() => {
  const files = Object.keys(canvas.boards);
  const order = (canvas.order ?? []).filter((f) => f in canvas.boards);
  const rest = files.filter((f) => !order.includes(f)).sort();
  return [...order, ...rest].map((file) => {
    const b = canvas.boards[file] ?? {};
    return {
      file,
      title: b.title ?? file.replace(/\.dc\.html$/, ""),
      page: b.page ?? "",
      width: b.w ?? 1440,
      height: b.h ?? 900,
    };
  });
})();

/** Canvas pages in order, each with its artboards. Pages without artboards are left out. */
export const pages: readonly Page[] = (() => {
  const known = canvas.pages ?? [];
  const ids = [...known.map((p) => p.id), ...new Set(boards.map((b) => b.page))];
  const seen = new Set<string>();
  const out: Page[] = [];
  for (const id of ids) {
    if (seen.has(id)) continue;
    seen.add(id);
    const list = boards.filter((b) => b.page === id);
    if (list.length) out.push({ id, name: known.find((p) => p.id === id)?.name ?? id, boards: list });
  }
  return out;
})();

export function findBoard(file: string): Board | undefined {
  return boards.find((b) => b.file === file);
}

/** Reads one artboard's source. */
export async function loadMock(file: string): Promise<string> {
  const load = sources[PREFIX + file];
  if (!load) throw new Error(`design/mocks/${file} doesn't exist. Check the file name, including .dc.html.`);
  return load();
}

/** Every .dc.html file in design/mocks, whether or not canvas.json lists it. */
export const mockFiles: readonly string[] = Object.keys(sources)
  .map((key) => key.slice(PREFIX.length))
  .sort();
