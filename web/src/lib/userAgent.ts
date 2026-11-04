// SPDX-License-Identifier: AGPL-3.0-only

/** A browser and an operating system, as people name them. */
export interface Device {
  /** "Chrome", "Safari", "Firefox", "Edge", "Opera", or "" when unknown. */
  browser: string;
  /** "macOS", "iOS", "iPadOS", "Android", "Windows", "Linux", "ChromeOS", or "" when unknown. */
  os: string;
  /** True for phones and tablets. */
  mobile: boolean;
}

// The first match wins, so each list goes from the most specific claim to the
// least: most browsers also call themselves Chrome or Safari.
const BROWSERS: [RegExp, string][] = [
  [/Edg(e|A|iOS)?\//, "Edge"],
  [/OPR\/|Opera/, "Opera"],
  [/Firefox\/|FxiOS\//, "Firefox"],
  [/Chrome\/|CriOS\/|Chromium\//, "Chrome"],
  [/Safari\//, "Safari"],
];

const SYSTEMS: [RegExp, string][] = [
  [/iPad/, "iPadOS"],
  [/iPhone|iPod/, "iOS"],
  [/Android/, "Android"],
  [/CrOS/, "ChromeOS"],
  [/Mac OS X|Macintosh/, "macOS"],
  [/Windows/, "Windows"],
  [/Linux|X11/, "Linux"],
];

const first = (rules: [RegExp, string][], ua: string) => rules.find(([re]) => re.test(ua))?.[1] ?? "";

/**
 * Reads a User-Agent header well enough to tell sessions and passkeys apart:
 * "Chrome on macOS", "Safari on iOS".
 */
export function describeUserAgent(ua: string): Device {
  return { browser: first(BROWSERS, ua), os: first(SYSTEMS, ua), mobile: /Mobile|iPhone|iPad|Android/.test(ua) };
}
