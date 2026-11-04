// SPDX-License-Identifier: AGPL-3.0-only

import { expect, it } from "vitest";

import { canvasBoards, canvasPages } from "./canvas";

it("lists artboards in canvas order, then the rest by name, each field defaulted on its own", () => {
  const boards = canvasBoards({
    boards: {
      "Zed.dc.html": { title: "Zed", page: "apps", w: 390, h: 844 },
      "Apps.dc.html": { page: "apps", w: 390 },
      "Tall.dc.html": { h: 1200 },
      "Main.dc.html": { title: "Map", page: "map" },
    },
    order: ["Main.dc.html", "Gone.dc.html", "Zed.dc.html", "Main.dc.html"],
  });
  expect(boards).toEqual([
    { file: "Main.dc.html", title: "Map", page: "map", width: 1440, height: 900 },
    { file: "Zed.dc.html", title: "Zed", page: "apps", width: 390, height: 844 },
    { file: "Apps.dc.html", title: "Apps", page: "apps", width: 390, height: 900 },
    { file: "Tall.dc.html", title: "Tall", page: "", width: 1440, height: 1200 },
  ]);
});

it("groups artboards by canvas page, in page order, leaving empty pages out", () => {
  const canvas = {
    boards: { "A.dc.html": { page: "apps" }, "B.dc.html": { page: "loose" }, "C.dc.html": { page: "map" } },
    pages: [
      { id: "map", name: "Screen map" },
      { id: "empty", name: "Nothing here" },
      { id: "apps", name: "Apps" },
    ],
  };
  const pages = canvasPages(canvas, canvasBoards(canvas));
  expect(pages.map((p) => [p.id, p.name, p.boards.map((b) => b.file)])).toEqual([
    ["map", "Screen map", ["C.dc.html"]],
    ["apps", "Apps", ["A.dc.html"]],
    ["loose", "loose", ["B.dc.html"]],
  ]);
});
