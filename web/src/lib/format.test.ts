// SPDX-License-Identifier: AGPL-3.0-only

import { afterEach, describe, expect, it } from "vitest";

import {
  activeTimeZone,
  dayDiff,
  dayKey,
  fileDay,
  formatBitRate,
  formatBytes,
  formatBytesOf,
  formatClock,
  formatClockSeconds,
  formatDateTime,
  formatDay,
  formatFixed,
  formatLiveBytesOf,
  formatMoment,
  formatPercent,
  formatTimeZoneOffset,
  formatWeekday,
  getDisplayTimeZone,
  sameDay,
  setDisplayTimeZone,
  timeZoneOffsetMinutes,
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
  const now = new Date(2026, 10, 4).getTime();

  it("puts the day first in English, and follows the locale elsewhere", () => {
    expect(formatDay(new Date(2026, 9, 12), "en", now)).toBe("12 Oct");
    expect(formatDay(new Date(2026, 9, 12), "zh-CN", now)).toBe("10月12日");
  });

  it("adds the year for another year", () => {
    expect(formatDay(new Date(2025, 9, 12), "en", now)).toBe("12 Oct 2025");
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

describe("formatWeekday and formatClock", () => {
  it("put the day before the month in English, and keep the 24-hour clock", () => {
    const date = new Date(2026, 10, 3, 9, 41);
    expect(formatWeekday(date, "en")).toBe("Tuesday 3 Nov");
    expect(formatClock(date, "en")).toBe("09:41");
  });

  it("keep another locale's own order", () => {
    const date = new Date(2026, 10, 3, 9, 41);
    expect(formatWeekday(date, "zh-CN")).toBe(
      new Intl.DateTimeFormat("zh-CN", { weekday: "long", day: "numeric", month: "short" })
        .format(date)
        .replace(",", ""),
    );
  });
});

describe("display time zone (#834)", () => {
  afterEach(() => {
    setDisplayTimeZone("");
  });

  it("formats dates, clocks, day keys, and year boundaries in the chosen time zone", () => {
    // 2026-12-31T20:30:05Z is 31 Dec 2026 in UTC / Honolulu, and 1 Jan 2027 05:30:05 in Asia/Tokyo.
    const instant = new Date("2026-12-31T20:30:05Z");
    const sameInstantUtcNow = new Date("2026-12-31T21:00:00Z").getTime();

    setDisplayTimeZone("Pacific/Honolulu");
    expect(getDisplayTimeZone()).toBe("Pacific/Honolulu");
    expect(activeTimeZone()).toBe("Pacific/Honolulu");
    expect(formatClock(instant, "en")).toBe("10:30");
    expect(formatClockSeconds(instant, "en")).toBe("10:30:05");
    expect(formatDateTime(instant, "en")).toBe("31 Dec 2026, 10:30");
    expect(formatWeekday(instant, "en")).toBe("Thursday 31 Dec");
    expect(fileDay(instant)).toBe("2026-12-31");
    expect(dayKey(instant)).toBe("2026-12-31");
    // In Honolulu, 2026-12-31T20:30Z and 2026-12-31T21:00Z are both in 2026.
    expect(formatDay(instant, "en", sameInstantUtcNow)).toBe("31 Dec");
    expect(formatMoment(instant, "en")).toContain("10:30:05");

    setDisplayTimeZone("Asia/Tokyo");
    expect(formatClock(instant, "en")).toBe("05:30");
    expect(formatClockSeconds(instant, "en")).toBe("05:30:05");
    expect(formatDateTime(instant, "en")).toBe("1 Jan 2027, 05:30");
    expect(formatWeekday(instant, "en")).toBe("Friday 1 Jan");
    expect(fileDay(instant)).toBe("2027-01-01");
    expect(dayKey(instant)).toBe("2027-01-01");
    // In Tokyo, 2026-12-31T10:00Z is still 31 Dec 2026, while instant is 1 Jan 2027 -> year included!
    expect(formatDay(instant, "en", new Date("2026-12-31T10:00:00Z").getTime())).toBe("1 Jan 2027");
    expect(sameDay(instant, new Date("2026-12-31T10:00:00Z"))).toBe(false);
    expect(dayDiff(instant, new Date("2026-12-31T10:00:00Z"))).toBe(1);

    // An explicit per-call override (such as a schedule's own zone) takes precedence.
    expect(formatDateTime(instant, "en", "UTC")).toBe("31 Dec 2026, 20:30");
    expect(dayKey(instant, "UTC")).toBe("2026-12-31");
  });

  it("computes UTC offsets accurately including half-hour zones", () => {
    const winter = new Date("2026-01-15T12:00:00Z");
    expect(timeZoneOffsetMinutes("UTC", winter)).toBe(0);
    expect(formatTimeZoneOffset("UTC", winter)).toBe("UTC");
    expect(timeZoneOffsetMinutes("Asia/Tokyo", winter)).toBe(540);
    expect(formatTimeZoneOffset("Asia/Tokyo", winter)).toBe("UTC+09:00");
    expect(timeZoneOffsetMinutes("America/Los_Angeles", winter)).toBe(-480);
    expect(formatTimeZoneOffset("America/Los_Angeles", winter)).toBe("UTC-08:00");
    expect(timeZoneOffsetMinutes("Asia/Kolkata", winter)).toBe(330);
    expect(formatTimeZoneOffset("Asia/Kolkata", winter)).toBe("UTC+05:30");
  });
});
