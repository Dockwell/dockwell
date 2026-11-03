// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { formatDateTime } from "../lib/format";
import { pickLocale } from "./i18n";

describe("pickLocale", () => {
  it.each([
    [["en-US", "zh-CN"], "en"],
    [["zh-CN", "en"], "zh-CN"],
    [["zh-Hans-CN"], "zh-CN"],
    [["zh-SG"], "zh-CN"],
    [["zh"], "zh-CN"],
    // Traditional Chinese readers get English until a translation exists.
    [["zh-TW", "fr"], "en"],
    [["fr-FR", "zh-CN"], "zh-CN"],
    [[], "en"],
  ])("%j → %s", (preferred, want) => {
    expect(pickLocale(preferred)).toBe(want);
  });
});

describe("formatDateTime", () => {
  const date = new Date(2026, 10, 3, 21, 40);

  it("writes day before month in English", () => {
    expect(formatDateTime(date, "en")).toBe("3 Nov 2026, 21:40");
  });

  it("uses the locale's own order elsewhere", () => {
    expect(formatDateTime(date, "zh-CN")).toBe("2026年11月3日 21:40");
  });
});
