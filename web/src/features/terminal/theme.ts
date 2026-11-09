// SPDX-License-Identifier: AGPL-3.0-only

import type { ResolvedTheme } from "@dockwell/ui";
import { colors, fonts } from "@dockwell/ui/tokens";
import type { ITheme } from "@xterm/xterm";

/** The terminal's type, as the mock sets it: JetBrains Mono, 12.5 on 20 on a desktop, 13 on 20 on a phone. */
export const TERMINAL_FONT = fonts.mono;
export const LINE_PX = 20;

/**
 * The line height xterm needs for rows LINE_PX apart. It multiplies the font's own
 * line box, as xterm measures it, so that's measured the same way: by the canvas's
 * text metrics where there are any.
 */
export function lineHeightFor(fontSize: number, family = TERMINAL_FONT): number {
  let natural = 0;
  try {
    const ctx = typeof OffscreenCanvas === "undefined" ? null : new OffscreenCanvas(1, 1).getContext("2d");
    if (ctx) {
      ctx.font = `${fontSize}px ${family}`;
      const m = ctx.measureText("W");
      if (m.fontBoundingBoxAscent !== undefined)
        natural = Math.ceil(m.fontBoundingBoxAscent + m.fontBoundingBoxDescent);
    }
  } catch {
    // No canvas here, as in a test.
  }
  if (!(natural > 0)) natural = Math.ceil(fontSize * 1.32);
  // A hair over, as xterm rounds a row down to whole pixels.
  return Math.max(1, (LINE_PX + 0.01) / natural);
}

/** Halfway between two #rrggbb colours. */
export function mix(a: string, b: string): string {
  const at = (hex: string, i: number) => Number.parseInt(hex.slice(i, i + 2), 16);
  return `#${[1, 3, 5]
    .map((i) => Math.round((at(a, i) + at(b, i)) / 2))
    .map((n) => n.toString(16).padStart(2, "0"))
    .join("")}`;
}

/**
 * The terminal's colours, in a theme. It's a well, so it's ink on the sunken
 * surface. The colours programs print map to the system's hues: red to critical,
 * green to healthy, yellow to warning, blue to cobalt. The system keeps no
 * magenta or cyan, as it keeps hues for meaning, so those are mixed from its own:
 * cobalt with critical, and cobalt with healthy. What a server's programs colour
 * is theirs to say, so these carry no meaning of Dockwell's.
 */
export function terminalTheme(theme: ResolvedTheme): ITheme {
  const c = colors[theme];
  const magenta = mix(c.cobalt, c.critical);
  const cyan = mix(c.cobalt, c.healthy);
  const light = theme === "light";
  return {
    foreground: c.ink,
    background: c["surface-sunken"],
    cursor: c.ink,
    cursorAccent: c["surface-sunken"],
    selectionBackground: c.selection,
    selectionForeground: c.ink,
    selectionInactiveBackground: c.selection,
    // What's printed black on a light well is ink, and white on a dark one is ink:
    // the colours a program means for the most text read as the most text does.
    black: light ? c.ink : c["line-strong"],
    brightBlack: c["ink-muted"],
    white: light ? c["ink-muted"] : c.ink,
    brightWhite: light ? c.ink : c["action-hover"],
    red: c.critical,
    brightRed: c.critical,
    green: c.healthy,
    brightGreen: c.healthy,
    yellow: c.warning,
    brightYellow: c.warning,
    blue: c.cobalt,
    brightBlue: c.cobalt,
    magenta,
    brightMagenta: magenta,
    cyan,
    brightCyan: cyan,
    scrollbarSliderBackground: `${c["line-strong"]}40`,
    scrollbarSliderHoverBackground: `${c["line-strong"]}80`,
    scrollbarSliderActiveBackground: `${c["line-strong"]}b3`,
  };
}
