// SPDX-License-Identifier: AGPL-3.0-only

import { fireEvent, render } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { AppIcon, artworkOf, enlarge, groundOf, leastFill, wordmarkFill } from "./media";

/** A square of side pixels, each from paint(x, y) as [r, g, b, a]. */
function square(side: number, paint: (x: number, y: number) => [number, number, number, number]): Uint8ClampedArray {
  const out = new Uint8ClampedArray(side * side * 4);
  for (let y = 0; y < side; y++) {
    for (let x = 0; x < side; x++) {
      out.set(paint(x, y), (y * side + x) * 4);
    }
  }
  return out;
}

const none: [number, number, number, number] = [0, 0, 0, 0];
const white: [number, number, number, number] = [255, 255, 255, 255];
const black: [number, number, number, number] = [20, 20, 20, 255];
const purple: [number, number, number, number] = [110, 70, 210, 255];

/** A mark in the middle of the square, on nothing. */
const mark = (colour: [number, number, number, number]) =>
  square(32, (x, y) => (x >= 8 && x < 24 && y >= 8 && y < 24 ? colour : none));

describe("groundOf", () => {
  it("puts a white mark on a dark tile, which a light one would hide", () => {
    expect(groundOf(mark(white), 32)).toBe("dark");
  });

  it("puts a dark or coloured mark on the light tile it was drawn for", () => {
    expect(groundOf(mark(black), 32)).toBe("light");
    expect(groundOf(mark(purple), 32)).toBe("light");
  });

  it("draws a logo that brings its own rounded square edge to edge, on no tile", () => {
    // Purple to the edges, its corners rounded off.
    const rounded = square(32, (x, y) => {
      const dx = Math.max(0, Math.abs(x - 15.5) - 10);
      const dy = Math.max(0, Math.abs(y - 15.5) - 10);
      return dx * dx + dy * dy <= 36 ? purple : none;
    });
    expect(groundOf(rounded, 32)).toBe("bleed");
    // A full square too.
    expect(
      groundOf(
        square(32, () => white),
        32,
      ),
    ).toBe("bleed");
  });

  it("reads a logo drawn only in faint pixels by them, and says one that draws nothing is empty", () => {
    const faint = (colour: [number, number, number, number]) =>
      square(32, (x, y) => (x >= 8 && x < 24 && y >= 8 && y < 24 ? colour : none));
    expect(groundOf(faint([40, 40, 40, 90]), 32)).toBe("light");
    expect(groundOf(faint([255, 255, 255, 90]), 32)).toBe("dark");
    expect(
      groundOf(
        square(32, () => none),
        32,
      ),
    ).toBe("empty");
  });
});

describe("artworkOf and enlarge", () => {
  it("finds a small mark's artwork past its margins, and draws it larger to fill 70% of its tile", () => {
    const art = artworkOf(mark(black), 32);
    expect(art).toEqual({ w: 0.5, h: 0.5, x: 0, y: 0 });
    // An SVG draws sharp at any size.
    expect(enlarge(art, 48, undefined) * 0.84 * 0.5).toBeCloseTo(leastFill);
    // A raster image large enough for it too.
    expect(enlarge(art, 48, 512) * 0.84 * 0.5).toBeCloseTo(leastFill);
    // A small raster image no further than its own pixels at twice the screen's.
    expect(enlarge(art, 48, 100)).toBeCloseTo(100 / (48 * 0.84 * 2));
    expect(enlarge(art, 48, 48)).toBe(1);
  });

  it("measures artwork without a stray dot far from it", () => {
    const dotted = square(32, (x, y) =>
      (x >= 12 && x < 20 && y >= 12 && y < 20) || (x === 30 && y === 30) ? black : none,
    );
    expect(artworkOf(dotted, 32)).toEqual({ w: 0.25, h: 0.25, x: 0, y: 0 });
  });

  it("draws a wide wordmark across more of its tile's width, and leaves artwork that fills its tile as it is", () => {
    // A wordmark the square's whole width, and 4 high.
    const wordmark = square(32, (_x, y) => (y >= 14 && y < 18 ? black : none));
    const art = artworkOf(wordmark, 32);
    expect(art).toMatchObject({ w: 1, h: 0.125 });
    expect(enlarge(art, 48, undefined) * 0.84).toBeCloseTo(wordmarkFill);
    expect(
      enlarge(
        artworkOf(
          square(32, () => black),
          32,
        ),
        48,
        undefined,
      ),
    ).toBe(1);
    expect(
      artworkOf(
        square(32, () => none),
        32,
      ),
    ).toBeUndefined();
  });
});

describe("AppIcon", () => {
  it("hides a logo until its ground is read, then shows it on the neutral tile where the page can't read it", () => {
    const { container } = render(<AppIcon name="Code server" path="/catalog-media/code-server.svg" />);
    const tile = container.querySelector(".catalog-icon");
    expect(tile).toHaveAttribute("data-ground", "reading");
    const img = container.querySelector("img");
    if (!img) throw new Error("no image");
    // This page draws nothing on a canvas, as a browser that refuses one doesn't.
    fireEvent.load(img);
    expect(tile).toHaveAttribute("data-ground", "neutral");
  });

  it("draws a monogram on no ground for an entry without artwork", () => {
    const { container } = render(<AppIcon name="Notes" path="" />);
    expect(container.querySelector(".catalog-icon")).not.toHaveAttribute("data-ground");
  });
});
