// SPDX-License-Identifier: AGPL-3.0-only

import { durationMs, timestampMs } from "@bufbuild/protobuf/wkt";
import type { ServerStats } from "@dockwell/api";

// The panel ships in the same binary as the control plane, so these match
// internal/control/fleet: how often agents send a sample, and how many
// samples each server keeps.

/** How often an agent sends a sample while anyone watches; every 10 s while nobody does. */
export const STATS_INTERVAL_MS = 5_000;
/** How many samples each server keeps for sparklines. */
export const HISTORY_LENGTH = 60;
/** How far back a history goes: 5 min. */
export const HISTORY_WINDOW_MS = HISTORY_LENGTH * STATS_INTERVAL_MS;

/**
 * How long a step between two samples can be, against the slower of their paces,
 * before it is a gap: one missed sample, as before.
 */
const GAP = 2.5;

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
  /** One sample for every STATS_INTERVAL_MS since the last gap in reporting, oldest first: the sample that covers that moment. */
  samples: readonly ServerStats[];
  /** The time they cover: one interval per sample, so 60 samples are 5 min. 0 for fewer than two. */
  spanMs: number;
}

/** How long a sample's rates cover: the pace it was taken at, 5 s from an agent from before it said. */
function intervalOf(s: ServerStats): number {
  return s.interval ? durationMs(s.interval) : STATS_INTERVAL_MS;
}

/**
 * The part of a history a trend can draw, on an even clock: one point for every
 * STATS_INTERVAL_MS back from the newest sample, each the sample that covers it,
 * since the agent last stopped reporting. Agents sample every 5 s while anyone
 * watches and every 10 s while nobody does, so a history holds both, and drawn one
 * point to a sample the slower part would look twice as quick. Before a gap
 * the line would join numbers either side of it as if they were continuous.
 */
export function trend(history: readonly ServerStats[]): RecentRun {
  const newest = history.at(-1);
  if (!newest?.time || history.some((s) => !s.time)) return { samples: newest ? [newest] : [], spanMs: 0 };
  const at = (s: ServerStats) => (s.time ? timestampMs(s.time) : 0);
  const end = at(newest);
  const samples: ServerStats[] = [];
  let j = history.length - 1;
  for (let slot = 0; slot < HISTORY_LENGTH; slot++) {
    const t = end - slot * STATS_INTERVAL_MS;
    // The earliest sample at or after t is the one whose rates cover it: back to the
    // sample before it, or after a gap or at the start, its own interval.
    while (j > 0 && at(history[j - 1] as ServerStats) >= t) j--;
    const s = history[j] as ServerStats;
    const prev = j > 0 ? (history[j - 1] as ServerStats) : undefined;
    // A step is judged by the slower of the two paces, as the pace changes between them
    // when someone starts or stops watching.
    const joined = prev !== undefined && at(s) - at(prev) <= Math.max(intervalOf(s), intervalOf(prev)) * GAP;
    const covers = at(s) >= t && (joined || at(s) - t < intervalOf(s));
    if (!covers) break;
    samples.push(s);
  }
  samples.reverse();
  return { samples, spanMs: samples.length > 1 ? samples.length * STATS_INTERVAL_MS : 0 };
}
