// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import {
  formatBitRate,
  formatBytes,
  formatBytesOf,
  formatDay,
  formatFixed,
  formatLiveBytesOf,
  formatPercent,
} from "./format";

const MiB = 1024 ** 2;
const GiB = 1024 ** 3;

describe("formatBytes", () => {
  it.each([
    [0, "0 B"],
    [512, "512 B"],
    [1024, "1 KB"],
    [512 * MiB, "512 MB"],
    [2.1 * GiB, "2.1 GB"],
    [8 * GiB, "8 GB"],
    [18.4 * GiB, "18 GB"],
    [1024 * GiB, "1 TB"],
    // Rounding up to the next unit moves to it.
    [1023.4 * 1024, "1,023 KB"],
    [MiB - 1, "1 MB"],
    [1023.6 * MiB, "1 GB"],
  ])("writes %d bytes as %s", (bytes, text) => {
    expect(formatBytes(bytes, "en")).toBe(text);
  });

  it("takes 64-bit sizes", () => {
    expect(formatBytes(BigInt(160 * GiB), "en")).toBe("160 GB");
  });
});

describe("formatBytesOf", () => {
  it("writes the part in the whole's unit", () => {
    expect(formatBytesOf(7.4 * GiB, 8 * GiB, "en")).toEqual(["7.4", "8 GB"]);
    expect(formatBytesOf(312 * MiB, 8 * GiB, "en")).toEqual(["0.3", "8 GB"]);
    expect(formatBytesOf(301 * MiB, 512 * MiB, "en")).toEqual(["301", "512 MB"]);
  });
});

describe("formatPercent", () => {
  it("rounds to a whole percentage in the locale's style", () => {
    expect(formatPercent(12.4, "en")).toBe("12%");
    expect(formatPercent(99.6, "zh-CN")).toBe("100%");
  });
});

describe("formatFixed", () => {
  it("keeps the decimals so live numbers don't change width", () => {
    expect(formatFixed(23, 1, "en")).toBe("23.0");
    expect(formatFixed(0.844, 2, "en")).toBe("0.84");
  });
});

describe("formatBitRate", () => {
  it.each([
    [0, "0", "bit/s"],
    [100, "800", "bit/s"],
    [1_550_000, "12", "Mbit/s"],
    [1_100_000 / 8, "1.1", "Mbit/s"],
    [62_500, "500", "kbit/s"],
    [2.5e9 / 8, "2.5", "Gbit/s"],
    // Rounding up to the next unit moves to it.
    [999_499 / 8, "999", "kbit/s"],
    [999_960 / 8, "1", "Mbit/s"],
    [999.6 / 8, "1", "kbit/s"],
  ])("writes %d bytes a second as %s %s", (bytes, value, unit) => {
    expect(formatBitRate(bytes, "en")).toEqual([value, unit]);
  });
});

describe("formatDay", () => {
  it("puts the day first in English, and follows the locale elsewhere", () => {
    expect(formatDay(new Date(2026, 9, 12), "en")).toBe("12 Oct");
    expect(formatDay(new Date(2026, 9, 12), "zh-CN")).toBe("10月12日");
  });
});

describe("formatLiveBytesOf", () => {
  it.each([
    [7 * GiB, 47 * GiB, "7.0", "47 GB"],
    [3.14 * GiB, 8 * GiB, "3.1", "8 GB"],
    [61 * GiB, 160 * GiB, "61", "160 GB"],
    [219.4 * GiB, 695 * GiB, "219", "695 GB"],
  ])("keeps %d of %d at a steady width", (part, whole, shown, total) => {
    expect(formatLiveBytesOf(part, whole, "en")).toEqual([shown, total]);
  });
});
