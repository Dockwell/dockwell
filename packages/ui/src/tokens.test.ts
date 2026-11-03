// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import { colors, type Theme } from "./tokens";

const repo = join(import.meta.dirname, "..", "..", "..");

/** Custom properties declared in the first rule whose selector matches. */
function declarations(css: string, selector: string): Map<string, string> {
  const start = css.indexOf(`${selector} {`);
  if (start < 0) throw new Error(`no rule for ${selector}`);
  const body = css.slice(start, css.indexOf("}", start));
  return new Map([...body.matchAll(/--([\w-]+):\s*([^;]+);/g)].map((m) => [m[1] ?? "", (m[2] ?? "").trim()]));
}

describe("generated tokens", () => {
  // design/system/tokens.css is Claude Design's own rendering of tokens.json.
  // Ours must agree with it value for value, in both themes.
  const reference = readFileSync(join(repo, "design/system/tokens.css"), "utf8");
  const ours = readFileSync(join(import.meta.dirname, "styles/tokens.css"), "utf8");

  for (const [theme, refSelector, ourSelector] of [
    ["light", ':root, [data-theme="light"]', ':root, [data-theme="light"]'],
    ["dark", '[data-theme="dark"]', '[data-theme="dark"]'],
  ] as const) {
    it(`match the design system's ${theme} theme`, () => {
      const want = declarations(reference, refSelector);
      const got = declarations(ours, ourSelector);
      expect(Object.fromEntries(got)).toEqual(Object.fromEntries(want));
    });
  }

  it("follow the system's dark preference unless light is chosen", () => {
    const media = ours.slice(ours.indexOf("@media (prefers-color-scheme: dark)"));
    expect(declarations(media, '  :root:not([data-theme="light"])')).toEqual(declarations(ours, '[data-theme="dark"]'));
  });
});

// WCAG 2.2 relative luminance and contrast ratio, for opaque #rrggbb colours.
function luminance(hex: string): number {
  const channels = [1, 3, 5].map((i) => Number.parseInt(hex.slice(i, i + 2), 16) / 255);
  const [r = 0, g = 0, b = 0] = channels.map((c) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

describe("contrast promised by the design system", () => {
  const surfaces = ["canvas", "surface", "surface-sunken", "surface-raised"] as const;
  const themes: Theme[] = ["light", "dark"];

  const pairs: [string, readonly string[], number][] = [
    // README: ink and ink-muted hold 4.5:1 on every surface token in both themes.
    ["ink", surfaces, 4.5],
    ["ink-muted", surfaces, 4.5],
    // line-strong holds 3:1 on canvas, surface and surface-raised (control borders).
    ["line-strong", ["canvas", "surface", "surface-raised"], 3],
    // Status hues are text on any surface or on their soft grounds.
    ...(["healthy", "warning", "critical", "cobalt"] as const).map(
      (hue) =>
        [hue, ["surface", "surface-raised", "canvas", `${hue}-soft`], 4.5] as [string, readonly string[], number],
    ),
    // Labels on fills.
    ["on-action", ["action"], 4.5],
    ["on-critical", ["critical"], 4.5],
    ["on-cobalt", ["cobalt"], 4.5],
  ];

  for (const theme of themes) {
    for (const [fg, grounds, min] of pairs) {
      for (const bg of grounds) {
        it(`${theme}: ${fg} on ${bg} is at least ${min}:1`, () => {
          const palette = colors[theme] as Record<string, string>;
          const ratio = contrast(palette[fg] ?? "", palette[bg] ?? "");
          expect(ratio, `${ratio.toFixed(2)}:1`).toBeGreaterThanOrEqual(min);
        });
      }
    }
  }
});
