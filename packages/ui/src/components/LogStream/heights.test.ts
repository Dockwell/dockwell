// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { HeightIndex } from "./heights";

describe("HeightIndex", () => {
  it("says where each row starts, and which row is where", () => {
    const h = new HeightIndex(4);
    for (const height of [19, 38, 19, 57, 19]) h.push(height);
    expect(h.total).toBe(152);
    expect(h.offset(0)).toBe(0);
    expect(h.offset(1)).toBe(19);
    expect(h.offset(4)).toBe(133);
    expect(h.at(0)).toBe(0);
    expect(h.at(18)).toBe(0);
    expect(h.at(19)).toBe(1);
    expect(h.at(56)).toBe(1);
    expect(h.at(57)).toBe(2);
    expect(h.at(151)).toBe(4);
    expect(h.at(10_000)).toBe(4);
  });

  it("takes a row's measured height", () => {
    const h = new HeightIndex();
    for (let i = 0; i < 10; i++) h.push(19);
    h.set(3, 95);
    expect(h.total).toBe(19 * 9 + 95);
    expect(h.offset(4)).toBe(19 * 3 + 95);
    expect(h.at(19 * 3 + 94)).toBe(3);
    expect(h.at(19 * 3 + 95)).toBe(4);
    expect(h.get(3)).toBe(95);
  });

  it("lets rows go from the start, and keeps their positions", () => {
    const h = new HeightIndex(8);
    for (let i = 0; i < 6; i++) h.push(10 + i);
    expect(h.dropTo(2)).toBe(21);
    expect(h.first).toBe(2);
    expect(h.length).toBe(4);
    expect(h.total).toBe(12 + 13 + 14 + 15);
    expect(h.offset(2)).toBe(0);
    expect(h.offset(3)).toBe(12);
    expect(h.at(0)).toBe(2);
    expect(h.at(12)).toBe(3);
  });

  it("stays right as it fills and starts over, far past its first size", () => {
    const h = new HeightIndex(16);
    const heights: number[] = [];
    let first = 0;
    for (let i = 0; i < 5000; i++) {
      const height = 19 * (1 + (i % 3));
      heights.push(height);
      h.push(height);
      if (i % 7 === 6) {
        h.dropTo(first + 5);
        first += 5;
      }
      if (i % 500 === 499) {
        h.set(first + 1, 200);
        heights[first + 1] = 200;
      }
    }
    expect(h.first).toBe(first);
    expect(h.length).toBe(5000 - first);
    let y = 0;
    for (let p = first; p < 5000; p += 97) {
      y = 0;
      for (let q = first; q < p; q++) y += heights[q] ?? 0;
      expect(h.offset(p)).toBe(y);
      expect(h.at(y)).toBe(p);
    }
    expect(h.total).toBe(heights.slice(first).reduce((a, b) => a + b, 0));
  });

  it("starts over on reset, from a position", () => {
    const h = new HeightIndex();
    h.push(19);
    h.reset(500);
    expect(h.total).toBe(0);
    expect(h.length).toBe(0);
    h.push(19);
    h.push(38);
    expect(h.first).toBe(500);
    expect(h.offset(501)).toBe(19);
    expect(h.at(20)).toBe(501);
  });
});
