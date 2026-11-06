// SPDX-License-Identifier: AGPL-3.0-only

import { hashKey, type QueryClient, type QueryKey, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useReducer } from "react";

import { type Unread, useOnline } from "./online";
import { ageOf, TRUST_FOR_MS, useWake } from "./wake";

/** How often a screen reads how things are, while it's open and in front: a third of how long a read stands. */
export const READ_EVERY_MS = 10_000;

/** How long one page of a read may take, so a read nobody answers gives way to the next. */
export const READ_TIMEOUT_MS = 8_000;

/** Why what was read no longer stands: this browser is offline, or the control plane doesn't answer. */
export interface Unstanding {
  why: Unread;
  /** When it was last heard, where it was. */
  heard: Date | undefined;
}

/** A read of how things are, as far as it can be believed now. */
export interface StandingRead<T> {
  /** What was read, while it stands. It's never there past TRUST_FOR_MS. */
  data: T | undefined;
  /**
   * What was last read, whatever its age. That's for what a read says of what exists,
   * such as a list's names, and never for how any of it is.
   */
  last: T | undefined;
  /** A read's first try is under way, within its time, so nobody here can say for a moment. */
  reading: boolean;
  /** Why nothing here can be said now, once that's so. */
  unread: Unstanding | undefined;
  /** What the last read failed with, while it's failing. */
  error: Error | null;
  /** Reads now, rather than at the next turn of the pace. */
  readAgain: () => void;
}

/** When a read was last heard, when its read under way began, and when one last failed, by a clock that only moves forward. */
interface Clocks {
  heard?: number;
  began?: number;
  /** On the same clock as heard, so a clock that steps can't make one look older than the last good read. */
  failed?: number;
}

const clocksByClient = new WeakMap<QueryClient, Map<string, Clocks>>();

/** One read's clocks, shared by every screen that shows it, as they share the read. */
function clocksOf(client: QueryClient, queryKey: QueryKey): Clocks {
  let byKey = clocksByClient.get(client);
  if (!byKey) {
    byKey = new Map();
    clocksByClient.set(client, byKey);
  }
  const key = hashKey(queryKey);
  let clocks = byKey.get(key);
  if (!clocks) {
    clocks = {};
    byKey.set(key, clocks);
  }
  return clocks;
}

/** Renders again in the soonest of ms, so a reading that runs out of time says so on its own. */
function useRenderIn(...ms: number[]): void {
  const [, tick] = useReducer((n: number) => n + 1, 0);
  const next = Math.min(...ms.filter((m) => m >= 0));
  // Set again on each render, as what's due moves with what's read.
  useEffect(() => {
    if (!Number.isFinite(next)) return;
    const timer = setTimeout(tick, next + 1);
    return () => clearTimeout(timer);
  });
}

/**
 * Reads how things are on a pace of its own, for an API with no stream of it, only
 * while a screen that shows it is open and in front (ADR 0027). What it last read stands
 * for TRUST_FOR_MS, whatever the reads do, by a clock that doesn't step back and one that
 * counts a sleep (ageOf), and read should give each call READ_TIMEOUT_MS, so one that
 * hangs gives way. Past that it never gives what it read as how things are: while the
 * first read back is under way, as on a screen back from the background or a laptop
 * that woke, it's reading, for that read's time at most, and then it's unread, and says
 * why.
 */
export function useStandingRead<T>({
  queryKey,
  read,
  every,
}: {
  queryKey: QueryKey;
  read: (signal: AbortSignal) => Promise<T>;
  /** How long until the next read, from what was last read. */
  every: (last: T | undefined) => number;
}): StandingRead<T> {
  const queryClient = useQueryClient();
  const online = useOnline();
  const clocks = clocksOf(queryClient, queryKey);
  const { data, dataUpdatedAt, error, fetchStatus, failureCount } = useQuery({
    queryKey,
    queryFn: async ({ signal }) => {
      // A read's first try starts its time; a try again after a failure doesn't.
      if ((queryClient.getQueryState(queryKey)?.fetchFailureCount ?? 0) === 0) clocks.began = performance.now();
      try {
        const out = await read(signal);
        clocks.heard = performance.now();
        return out;
      } catch (err) {
        clocks.failed = performance.now();
        throw err;
      }
    },
    staleTime: 0,
    refetchInterval: (query) => every(query.state.data),
  });
  const readAgain = () => void queryClient.refetchQueries({ queryKey, type: "active" }, { cancelRefetch: false });
  // Coming back into view, waking, or a clock that steps, looks again, and reads again,
  // as no timer that was waiting ran.
  const [, look] = useReducer((n: number) => n + 1, 0);
  useWake(() => {
    look();
    readAgain();
  });
  const age = data === undefined ? Number.POSITIVE_INFINITY : ageOf(dataUpdatedAt, clocks.heard);
  const trusted = age <= TRUST_FOR_MS;
  // A read's first try under way, within its time, is reading, and never what was read,
  // until a read has failed since the last that didn't: then nobody can say, and stays so,
  // rather than come and go with each read that tries again.
  // What hasn't begun yet is about to: a screen's first render comes before its read does.
  const tried = clocks.began === undefined ? 0 : performance.now() - clocks.began;
  const reading =
    !trusted &&
    fetchStatus === "fetching" &&
    failureCount === 0 &&
    (clocks.failed ?? Number.NEGATIVE_INFINITY) <= (clocks.heard ?? Number.NEGATIVE_INFINITY) &&
    tried < READ_TIMEOUT_MS;
  useRenderIn(trusted ? TRUST_FOR_MS - age : Number.NaN, reading ? READ_TIMEOUT_MS - tried : Number.NaN);
  const why: Unread = online ? "unreachable" : "offline";
  const lastHeard = dataUpdatedAt > 0 ? dataUpdatedAt : undefined;
  const lost = !trusted && !reading;
  const unread = useMemo(
    () => (lost ? { why, heard: lastHeard === undefined ? undefined : new Date(lastHeard) } : undefined),
    [lost, why, lastHeard],
  );
  return { data: trusted ? data : undefined, last: data, reading, unread, error, readAgain };
}
