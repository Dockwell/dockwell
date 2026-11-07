// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { gap, history, sample } from "../../test/servers";
import { appendSample, HISTORY_LENGTH, HISTORY_WINDOW_MS, trend } from "./history";

const cpu = (samples: readonly { cpuPercent: number }[]) => samples.map((s) => s.cpuPercent);

describe("appendSample", () => {
  it("keeps at most HISTORY_LENGTH samples", () => {
    let kept = history(...Array.from({ length: HISTORY_LENGTH }, (_, i) => i));
    kept = appendSample(kept, sample(99));
    expect(kept).toHaveLength(HISTORY_LENGTH);
    expect(kept[0]?.cpuPercent).toBe(1);
    expect(kept.at(-1)?.cpuPercent).toBe(99);
  });

  it("starts over when the agent reports again after a longer gap than the window", () => {
    const before = history(1, 2, 3);
    gap(2 * 60 * 60_000);
    expect(cpu(appendSample(before, sample(50)))).toEqual([50]);
  });

  it("keeps the samples still inside the window after a shorter gap", () => {
    const before = history(1, 2, 3);
    gap(HISTORY_WINDOW_MS - 15_000);
    // The newest is a whole window after the first, which drops out; the others are inside it.
    expect(cpu(appendSample(before, sample(50)))).toEqual([2, 3, 50]);
  });
});

describe("trend", () => {
  it("covers one interval per sample, so a full history is 5 min", () => {
    const full = history(...Array.from({ length: HISTORY_LENGTH }, (_, i) => i));
    const run = trend(full);
    expect(run.samples).toHaveLength(HISTORY_LENGTH);
    expect(run.spanMs).toBe(5 * 60_000);
  });

  it("starts after the last gap", () => {
    const before = history(1, 2, 3);
    gap(60_000);
    const after = history(4, 5);
    const run = trend([...before, ...after]);
    expect(cpu(run.samples)).toEqual([4, 5]);
    expect(run.spanMs).toBe(10_000);
  });

  it("bridges one missed sample with the next, whose rates cover both", () => {
    const before = history(1, 2);
    gap(5_000);
    expect(cpu(trend([...before, ...history(3)]).samples)).toEqual([1, 2, 3, 3]);
  });

  it("draws samples taken every 15 s, while nobody watched, on the same clock as those every 5 s", () => {
    const slow = [sample(10, 2.4, 76, 15_000), sample(20, 2.4, 76, 15_000)];
    const fast = [sample(30), sample(40)];
    // 15 s, then two 5 s steps: each slow sample covers the 15 s before it.
    expect(cpu(trend([...slow, ...fast]).samples)).toEqual([10, 10, 10, 20, 20, 20, 30, 40]);
  });

  it("joins the first sample after someone starts watching to the slow ones before it", () => {
    const slow = [sample(10, 2.4, 76, 15_000), sample(20, 2.4, 76, 15_000)];
    // The agent sends one at once as it speeds up: 14 s after the last, at the new pace.
    gap(9_000);
    const quick = sample(30);
    expect(cpu(trend([...slow, quick]).samples)).toEqual([10, 10, 10, 20, 20, 20, 30, 30, 30]);
  });

  it("joins the first sample after everyone stops watching to the quick ones before it", () => {
    const quick = [sample(10), sample(20)];
    // The next comes 15 s later, at the new pace.
    expect(cpu(trend([...quick, sample(30, 2.4, 76, 15_000)]).samples)).toEqual([10, 20, 30, 30, 30]);
  });

  it("starts after a gap longer than a slow sample covers", () => {
    const slow = [sample(10, 2.4, 76, 15_000)];
    gap(30_000);
    expect(cpu(trend([...slow, sample(20, 2.4, 76, 15_000)]).samples)).toEqual([20, 20, 20]);
  });

  it("gives no span for a single sample", () => {
    expect(trend(history(7))).toEqual({ samples: [expect.objectContaining({ cpuPercent: 7 })], spanMs: 0 });
    expect(trend([])).toEqual({ samples: [], spanMs: 0 });
  });
});
