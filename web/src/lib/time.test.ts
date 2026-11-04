// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { renderHookWithApi } from "../test/render";
import { useTimeWords } from "./time";

const words = () => renderHookWithApi(useTimeWords, {}).result.current;
const NOW = new Date(2026, 10, 4, 12, 0).getTime();
const before = (ms: number) => new Date(NOW - ms);

describe("useTimeWords", () => {
  it.each([
    [0, "1 s ago"],
    [400, "1 s ago"],
    [-2_000, "1 s ago"],
    [7_900, "7 s ago"],
    [3 * 60_000, "3 min ago"],
    [2 * 3_600_000, "2 h ago"],
    [3 * 86_400_000, "1 Nov"],
  ])("says %d ms ago as “%s”", (ms, text) => {
    expect(words().ago(before(ms), NOW)).toBe(text);
  });

  it.each([
    [40_000, "40 s"],
    [12 * 60_000, "12 min"],
    [5 * 3_600_000, "5 hours"],
    [41 * 86_400_000, "41 days"],
  ])("says a span of %d ms as “%s”", (ms, text) => {
    expect(words().span(ms)).toBe(text);
  });
});
