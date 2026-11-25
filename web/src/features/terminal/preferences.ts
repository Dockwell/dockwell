// SPDX-License-Identifier: AGPL-3.0-only

import { TerminalCursorStyle, type TerminalPreferences } from "@dockwell/api";
import { fonts } from "@dockwell/ui/tokens";

import { lineHeightFor, TERMINAL_FONT } from "./theme";

/** Default font sizes matching the mocks: 12.5 px on desktop, 13 px on a phone. */
export const DESKTOP_FONT = 12.5;
export const PHONE_FONT = 13;

/** Sizes the phone's Aa button cycles through, starting from the mock's. */
export const PHONE_SIZES = [13, 15, 17, 11] as const;

export const DEFAULT_SCROLLBACK = 5000;
export const MIN_FONT_SIZE = 10;
export const MAX_FONT_SIZE = 24;
export const MIN_SCROLLBACK = 500;
export const MAX_SCROLLBACK = 50000;

export type FontFamilyKey = "default" | "system-mono" | "fira-code" | "ibm-plex-mono" | "source-code-pro";

export interface FontPreset {
  key: FontFamilyKey;
  css: string;
}

/** Terminal font presets, with the panel's own monospace font first. */
export const FONT_PRESETS: readonly FontPreset[] = [
  { key: "default", css: TERMINAL_FONT },
  {
    key: "system-mono",
    css: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace',
  },
  { key: "fira-code", css: `"Fira Code", ${fonts.mono}` },
  { key: "ibm-plex-mono", css: `"IBM Plex Mono", ${fonts.mono}` },
  { key: "source-code-pro", css: `"Source Code Pro", ${fonts.mono}` },
];

export const LINE_HEIGHT_PRESETS = [0, 1.0, 1.2, 1.4, 1.6, 1.8] as const;
export const SCROLLBACK_PRESETS = [1000, 5000, 10000, 25000, 50000] as const;

export type XtermCursorStyle = "block" | "underline" | "bar";

export interface ResolvedDisplaySettings {
  fontSize: number;
  fontFamilyKey: FontFamilyKey;
  fontFamily: string;
  /** Effective lineHeight multiplier passed to xterm. */
  lineHeight: number;
  /** Raw stored lineHeight (0 = auto). */
  lineHeightSetting: number;
  cursorStyle: XtermCursorStyle;
  cursorStyleEnum: TerminalCursorStyle;
  cursorBlink: boolean;
  scrollback: number;
}

export function fontFamilyKeyOf(raw: string | undefined): FontFamilyKey {
  if (!raw || raw === "default") return "default";
  const found = FONT_PRESETS.find((p) => p.key === raw);
  return found ? found.key : "default";
}

export function fontFamilyCssOf(raw: string | undefined): string {
  const key = fontFamilyKeyOf(raw);
  return FONT_PRESETS.find((p) => p.key === key)?.css ?? TERMINAL_FONT;
}

export function cursorStyleToXterm(style: TerminalCursorStyle | undefined): XtermCursorStyle {
  switch (style) {
    case TerminalCursorStyle.UNDERLINE:
      return "underline";
    case TerminalCursorStyle.BAR:
      return "bar";
    default:
      return "block";
  }
}

export function xtermToCursorStyle(style: XtermCursorStyle): TerminalCursorStyle {
  switch (style) {
    case "underline":
      return TerminalCursorStyle.UNDERLINE;
    case "bar":
      return TerminalCursorStyle.BAR;
    default:
      return TerminalCursorStyle.BLOCK;
  }
}

/**
 * Resolves effective xterm display settings from saved user preferences,
 * preserving the mock defaults and allowing the phone's Aa button to override
 * the font size locally on that device.
 */
export function resolveDisplaySettings(
  prefs: TerminalPreferences | undefined,
  phone: boolean,
  phoneFontOverride?: number,
): ResolvedDisplaySettings {
  const storedSize =
    prefs?.fontSize && prefs.fontSize >= MIN_FONT_SIZE && prefs.fontSize <= MAX_FONT_SIZE ? prefs.fontSize : 0;
  const fontSize = phone ? (phoneFontOverride ?? (storedSize || PHONE_FONT)) : storedSize || DESKTOP_FONT;
  const fontFamilyKey = fontFamilyKeyOf(prefs?.fontFamily);
  const fontFamily = fontFamilyCssOf(fontFamilyKey);
  const lineHeightSetting =
    prefs?.lineHeight && prefs.lineHeight >= 1.0 && prefs.lineHeight <= 2.0 ? prefs.lineHeight : 0;
  const lineHeight = lineHeightSetting > 0 ? lineHeightSetting : lineHeightFor(fontSize, fontFamily);
  const cursorStyleEnum = prefs?.cursorStyle || TerminalCursorStyle.BLOCK;
  const cursorStyle = cursorStyleToXterm(cursorStyleEnum);
  const cursorBlink = prefs?.cursorBlink ?? true;
  const scrollback =
    prefs?.scrollback && prefs.scrollback >= MIN_SCROLLBACK && prefs.scrollback <= MAX_SCROLLBACK
      ? prefs.scrollback
      : DEFAULT_SCROLLBACK;
  return {
    fontSize,
    fontFamilyKey,
    fontFamily,
    lineHeight,
    lineHeightSetting,
    cursorStyle,
    cursorStyleEnum,
    cursorBlink,
    scrollback,
  };
}
