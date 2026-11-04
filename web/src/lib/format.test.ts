// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { formatBytes, formatBytesOf, formatPercent } from "./format";

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
