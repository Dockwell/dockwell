// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import type { ServerStats } from "@dockwell/api";

// The panel ships in the same binary as the control plane, so these match
// internal/control/fleet: how often agents send a sample, and how many
// samples each server keeps.

/** How often an agent sends a sample. */
export const STATS_INTERVAL_MS = 5_000;
/** How many samples each server keeps for sparklines. */
export const HISTORY_LENGTH = 60;
/** How far back a history goes: 5 min. */
export const HISTORY_WINDOW_MS = HISTORY_LENGTH * STATS_INTERVAL_MS;

/** A pause in reporting longer than this, more than one missed sample, breaks a trend. */
const GAP_MS = 2.5 * STATS_INTERVAL_MS;

/**
 * Adds the newest sample to a history the way the control plane keeps it:
 * oldest first, at most HISTORY_LENGTH, and nothing from more than the
 * window before the newest, measured on the samples' own clock. So once a
 * server reports again after an outage, the history starts over.
 */
export function appendSample(history: readonly ServerStats[], sample: ServerStats): ServerStats[] {
  const at = sample.time ? timestampMs(sample.time) : undefined;
  const kept =
    at === undefined ? history : history.filter((s) => !s.time || timestampMs(s.time) > at - HISTORY_WINDOW_MS);
  return [...kept, sample].slice(-HISTORY_LENGTH);
}

export interface RecentRun {
  /** The samples since the last gap in reporting, oldest first. */
  samples: readonly ServerStats[];
  /** The time they cover: one interval per sample, so 60 samples are 5 min. 0 for fewer than two. */
  spanMs: number;
}

/**
 * The part of a history a trend can draw: the samples since the agent last
 * stopped reporting. Before that the line would join numbers either side of
 * the gap as if they were continuous.
 */
export function recentRun(history: readonly ServerStats[]): RecentRun {
  let start = history.length - 1;
  while (start > 0) {
    const before = history[start - 1]?.time;
    const after = history[start]?.time;
    if (!before || !after) break;
    const step = timestampMs(after) - timestampMs(before);
    if (step <= 0 || step > GAP_MS) break;
    start--;
  }
  const samples = history.slice(Math.max(start, 0));
  return { samples, spanMs: samples.length > 1 ? samples.length * STATS_INTERVAL_MS : 0 };
}
