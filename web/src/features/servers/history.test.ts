// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { gap, history, sample } from "../../test/servers";
import { appendSample, HISTORY_LENGTH, HISTORY_WINDOW_MS, recentRun } from "./history";

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

describe("recentRun", () => {
  it("covers one interval per sample, so a full history is 5 min", () => {
    const full = history(...Array.from({ length: HISTORY_LENGTH }, (_, i) => i));
    const run = recentRun(full);
    expect(run.samples).toHaveLength(HISTORY_LENGTH);
    expect(run.spanMs).toBe(5 * 60_000);
  });

  it("starts after the last gap", () => {
    const before = history(1, 2, 3);
    gap(60_000);
    const after = history(4, 5);
    const run = recentRun([...before, ...after]);
    expect(cpu(run.samples)).toEqual([4, 5]);
    expect(run.spanMs).toBe(10_000);
  });

  it("bridges one missed sample", () => {
    const before = history(1, 2);
    gap(5_000);
    expect(cpu(recentRun([...before, ...history(3)]).samples)).toEqual([1, 2, 3]);
  });

  it("gives no span for a single sample", () => {
    expect(recentRun(history(7))).toEqual({ samples: [expect.objectContaining({ cpuPercent: 7 })], spanMs: 0 });
    expect(recentRun([])).toEqual({ samples: [], spanMs: 0 });
  });
});
