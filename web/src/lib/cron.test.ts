// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { nextRuns, parseCron, type Schedule } from "./cron";

/** A schedule the test needs to read, or a failure that says which. */
function must(expr: string): Schedule {
  const s = parseCron(expr);
  if (!s) throw new Error(`${expr} didn't parse`);
  return s;
}

/** What internal/cron says of each, the next four runs after a time, as its Next gives them. */
const FROM_GO = [
  // Every five minutes runs by the real time.
  [
    "*/5 * * * *",
    "Australia/Lord_Howe",
    "2026-03-29T00:30:00Z",
    ["00:35", "00:40", "00:45", "00:50"].map((m) => `2026-03-29T${m}:00Z`),
  ],
  // 02:30 doesn't happen on 8 March in Los Angeles: it runs at the minute the clocks jumped to.
  [
    "30 2 * * *",
    "America/Los_Angeles",
    "2026-03-08T09:30:00Z",
    ["2026-03-08T10:00:00Z", "2026-03-09T09:30:00Z", "2026-03-10T09:30:00Z", "2026-03-11T09:30:00Z"],
  ],
  // 02:30 happens twice on 25 October in Berlin: it runs the first time only.
  [
    "30 2 * * *",
    "Europe/Berlin",
    "2026-10-25T00:10:00Z",
    ["2026-10-25T00:30:00Z", "2026-10-26T01:30:00Z", "2026-10-27T01:30:00Z", "2026-10-28T01:30:00Z"],
  ],
  // A half-hour zone, weekdays only.
  [
    "0 4 * * 1-5",
    "Asia/Kolkata",
    "2026-11-10T10:07:00Z",
    ["2026-11-10T22:30:00Z", "2026-11-11T22:30:00Z", "2026-11-12T22:30:00Z", "2026-11-15T22:30:00Z"],
  ],
  // Both day fields set: either one is enough, as in Vixie cron.
  [
    "0 0 13 * 5",
    "UTC",
    "2026-11-10T10:07:00Z",
    ["2026-11-13T00:00:00Z", "2026-11-20T00:00:00Z", "2026-11-27T00:00:00Z", "2026-12-04T00:00:00Z"],
  ],
  [
    "0 9 * JAN,JUL MON",
    "Europe/Berlin",
    "2026-11-10T10:07:00Z",
    ["2027-01-04T08:00:00Z", "2027-01-11T08:00:00Z", "2027-01-18T08:00:00Z", "2027-01-25T08:00:00Z"],
  ],
  // Set hours through an hour that comes twice: the first of each wall-clock minute.
  [
    "*/30 1-3 * * *",
    "America/Los_Angeles",
    "2026-11-01T08:20:00Z",
    ["2026-11-01T08:30:00Z", "2026-11-01T10:00:00Z", "2026-11-01T10:30:00Z", "2026-11-01T11:00:00Z"],
  ],
  // Months without a 31st are skipped.
  [
    "0 4 31 * *",
    "UTC",
    "2026-11-10T10:07:00Z",
    ["2026-12-31T04:00:00Z", "2027-01-31T04:00:00Z", "2027-03-31T04:00:00Z", "2027-05-31T04:00:00Z"],
  ],
] as const;

describe("cron, as the control plane reads it", () => {
  it.each(FROM_GO)("%s in %s after %s runs when internal/cron says", (expr, zone, after, want) => {
    const got = nextRuns(must(expr), Date.parse(after), zone, 4).map((ms) =>
      new Date(ms).toISOString().replace(".000Z", "Z"),
    );
    expect(got).toEqual(want);
  });

  it("reads names, lists, steps, Sunday as 7 and the @ words", () => {
    expect(parseCron("0 9 * jan,Jul mon-fri")?.month).toEqual(new Set([1, 7]));
    expect(parseCron("5/15 * * * *")?.minute).toEqual(new Set([5, 20, 35, 50]));
    expect(parseCron("0 0 * * 7")?.dow).toEqual(new Set([0]));
    expect(parseCron("@daily")?.hour).toEqual(new Set([0]));
  });

  it.each([
    "",
    "every day",
    "0 4 * *",
    "60 * * * *",
    "0 4 * * 8",
    "5-1 * * * *",
    "*/0 * * * *",
    "1,,2 * * * *",
    "@reboot",
  ])("refuses %j, as the control plane would", (expr) => {
    expect(parseCron(expr)).toBeUndefined();
  });

  it("says nothing runs for a date that never comes", () => {
    expect(nextRuns(must("0 0 30 2 *"), Date.parse("2026-11-10T00:00:00Z"), "UTC", 3)).toEqual([]);
  });
});
