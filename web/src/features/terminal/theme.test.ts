// SPDX-License-Identifier: AGPL-3.0-only

import { colors } from "@dockwell/ui/tokens";
import { describe, expect, it } from "vitest";

import { lineHeightFor, mix, terminalTheme } from "./theme";

function luminance(hex: string): number {
  const channels = [1, 3, 5].map((i) => Number.parseInt(hex.slice(i, i + 2), 16) / 255);
  const [r = 0, g = 0, b = 0] = channels.map((c) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

describe("the terminal's colours", () => {
  for (const theme of ["light", "dark"] as const) {
    it(`read on the well in the ${theme} theme`, () => {
      const t = terminalTheme(theme);
      expect(t.background).toBe(colors[theme]["surface-sunken"]);
      expect(t.foreground).toBe(colors[theme].ink);
      // What programs print most in colour holds the 4.5:1 the system holds its text to.
      for (const name of ["foreground", "red", "green", "yellow", "blue", "magenta", "cyan", "brightBlack"] as const) {
        const value = t[name] ?? "";
        expect(contrast(value, t.background ?? ""), `${name} ${value}`).toBeGreaterThanOrEqual(4.5);
      }
      // The colour a program means for the most text reads as the most text does.
      expect(
        contrast(theme === "light" ? (t.black ?? "") : (t.white ?? ""), t.background ?? ""),
      ).toBeGreaterThanOrEqual(4.5);
    });
  }

  it("mixes what the system keeps no hue for from its own", () => {
    expect(mix("#000000", "#ffffff")).toBe("#808080");
    expect(terminalTheme("light").magenta).toBe(mix(colors.light.cobalt, colors.light.critical));
    expect(terminalTheme("dark").cyan).toBe(mix(colors.dark.cobalt, colors.dark.healthy));
  });

  it("sets rows 20 px apart whatever the font's own line box", () => {
    // jsdom has no canvas, so the font's line box is taken as 1.32 of its size.
    expect(Math.floor(Math.ceil(12.5 * 1.32) * lineHeightFor(12.5))).toBe(20);
    expect(Math.floor(Math.ceil(13 * 1.32) * lineHeightFor(13))).toBe(20);
  });
});
