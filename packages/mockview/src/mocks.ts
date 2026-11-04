// SPDX-License-Identifier: AGPL-3.0-only

// The mocks in design/mocks, read where they are: nothing is copied.

import canvasJson from "../../../design/mocks/canvas.json";
import { type Board, type Canvas, canvasBoards, canvasPages, type Page } from "./canvas";

const sources = import.meta.glob<string>("../../../design/mocks/*.dc.html", { query: "?raw", import: "default" });
const PREFIX = "../../../design/mocks/";
const canvas = canvasJson as unknown as Canvas;

/** Every artboard in canvas order, then any the order leaves out. */
export const boards: readonly Board[] = canvasBoards(canvas);

/** Canvas pages in order, each with its artboards. Pages without artboards are left out. */
export const pages: readonly Page[] = canvasPages(canvas, boards);

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
