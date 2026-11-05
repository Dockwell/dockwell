// SPDX-License-Identifier: AGPL-3.0-only

import { type Timestamp, timestampMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { Connection, type Server, ServerService } from "@dockwell/api";
import { createContext, createElement, type ReactNode, useContext, useEffect, useRef, useState } from "react";

import { isSessionEnded } from "../../lib/errors";
import { type Unread, useOnline } from "../../lib/online";
import { keepTrying } from "../../lib/retry";
import { useNow, useTickingNow } from "../../lib/time";
import { ageOf, TRUST_FOR_MS, useWake } from "../../lib/wake";
import { appendSample } from "./history";
import { type FleetView, fleetView } from "./server";

export type FleetState =
  | { status: "connecting"; error?: ConnectError }
  | { status: "live"; servers: Server[] }
  | { status: "reconnecting"; servers: Server[]; error: ConnectError };

const BACKOFF = { minMs: 1_000, maxMs: 15_000 };

/** How often a watch looks for having heard nothing for too long. */
const STALL_LOOK_MS = 5_000;

/** What a watch that heard nothing for TRUST_FOR_MS ends with: the control plane sends a heartbeat every 10 s. */
export const STALLED = new ConnectError("the control plane has said nothing for 30 s", Code.DeadlineExceeded);

/**
 * When the watch last heard anything, a heartbeat or a change, by the wall clock and
 * one that only moves forward. It is kept outside React, so a sample renders nothing
 * more, and read by what says whether what the fleet last said still stands.
 */
interface Heard {
  wall: number;
  mono: number | undefined;
}

/**
 * A watch's state; the shell's view of it, which keeps its identity while
 * only the numbers change; and how far the browser's clock runs ahead of
 * the control plane's.
 */
interface Watched {
  state: FleetState;
  view: FleetView;
  skewMs: number;
  heard: Heard;
}

// Separate contexts, so what reads only the view isn't woken by each sample.
const StateContext = createContext<FleetState | undefined>(undefined);
const ViewContext = createContext<FleetView | undefined>(undefined);
const SkewContext = createContext(0);
const HeardContext = createContext<Heard | undefined>(undefined);

/**
 * Watches the fleet once for everything inside it, so the shell and the
 * screen it holds share one stream.
 */
export function FleetProvider({ children }: { children: ReactNode }) {
  const { state, view, skewMs, heard } = useWatch(true);
  return createElement(
    StateContext,
    { value: state },
    createElement(
      ViewContext,
      { value: view },
      createElement(SkewContext, { value: skewMs }, createElement(HeardContext, { value: heard }, children)),
    ),
  );
}

/**
 * Why what the fleet last said can't stand, and when it was last heard, once the
 * watch has heard nothing, not even a heartbeat, for TRUST_FOR_MS: as a frozen
 * control plane, a proxy that holds the stream open, or a browser gone offline leaves
 * it. Until then, nothing. It looks again on its own, and as the page comes back or
 * the machine wakes, and renders only when that changes.
 */
export function useFleetUnread(): { why: Unread; heard: Date } | undefined {
  const heard = useContext(HeardContext);
  const online = useOnline();
  const [unread, setUnread] = useState(false);
  const look = useRef<() => number>(() => Number.POSITIVE_INFINITY);
  look.current = () => {
    if (heard?.mono === undefined) {
      setUnread(false);
      return Number.POSITIVE_INFINITY;
    }
    const age = ageOf(heard.wall, heard.mono);
    setUnread(age > TRUST_FOR_MS);
    return TRUST_FOR_MS - age;
  };
  useWake(() => look.current());
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const again = () => {
      const left = look.current();
      // While it can't stand, or nothing has come yet, it looks each second.
      timer = setTimeout(again, (Number.isFinite(left) ? Math.max(left, 1_000) : 1_000) + 50);
    };
    again();
    return () => clearTimeout(timer);
  }, []);
  if (!unread || heard === undefined) return undefined;
  return { why: online ? "unreachable" : "offline", heard: new Date(heard.wall) };
}

/**
 * Now on the control plane's clock, as well as the browser can tell, kept
 * up to date every interval. Server timestamps, such as when an agent last
 * reported, are on that clock, so how long ago they were is this less the
 * timestamp; the browser's own clock may be minutes off.
 */
export function useControlPlaneNow(interval?: number): number {
  return useNow(interval) - useContext(SkewContext);
}

/**
 * The time on the control plane's clock, for a screen that shows how long ago
 * things were, the newest of them being newest. It ticks by the second only
 * while that is under a minute old (useTickingNow).
 */
export function useAgoNow(newest: Date | undefined): number {
  const skew = useContext(SkewContext);
  return useTickingNow(newest === undefined ? undefined : newest.getTime() + skew) - skew;
}

/**
 * Every server and its live stats, from ServerService.WatchServers. The
 * stream opens with a snapshot, each server with its history, and marks its
 * end; later messages carry only the newest sample, appended here. A dropped
 * stream reconnects with backoff, keeps showing what it knew, and swaps in
 * the fresh snapshot once it is complete. Inside a FleetProvider this reads
 * the provider's watch; anywhere else it opens its own.
 */
export function useFleet(): FleetState {
  const shared = useContext(StateContext);
  const own = useWatch(shared === undefined);
  return shared ?? own.state;
}

/**
 * The fleet as the shell shows it: how many servers, their names and how
 * they are. Unlike useFleet, a component that reads only this doesn't
 * render again for each stats sample.
 */
export function useFleetView(): FleetView {
  const shared = useContext(ViewContext);
  const own = useWatch(shared === undefined);
  return shared ?? own.view;
}

const CONNECTING: FleetState = { status: "connecting" };

function useWatch(enabled: boolean): Watched {
  const transport = useTransport();
  const heard = useRef<Heard>({ wall: 0, mono: undefined }).current;
  const [watched, setWatched] = useState<Watched>(() => ({
    state: CONNECTING,
    view: fleetView(CONNECTING),
    skewMs: 0,
    heard,
  }));

  useEffect(() => {
    if (!enabled) return;
    const abort = new AbortController();
    void watch(transport, abort.signal, heard, (state, skewMs) =>
      setWatched((prev) => ({ state, view: fleetView(state, prev.view), skewMs, heard })),
    );
    return () => abort.abort();
  }, [transport, enabled, heard]);

  return watched;
}

async function watch(
  transport: Transport,
  signal: AbortSignal,
  heard: Heard,
  publish: (s: FleetState, skewMs: number) => void,
): Promise<void> {
  const client = createClient(ServerService, transport);
  const clock = new SkewEstimate();
  let known: Server[] | undefined;
  await keepTrying(
    signal,
    BACKOFF,
    async (connected, attempt) => {
      const servers = new Map<string, Server>();
      let synced = false;
      // A try that hears nothing for TRUST_FOR_MS, since it began or since it last
      // heard, has stalled, and makes way for the next.
      const stalled = new AbortController();
      const stop = () => stalled.abort(attempt.reason);
      attempt.addEventListener("abort", stop, { once: true });
      const began = { wall: Date.now(), mono: performance.now() };
      const looking = setInterval(() => {
        const last =
          heard.mono !== undefined && heard.mono > began.mono ? { wall: heard.wall, mono: heard.mono } : began;
        if (ageOf(last.wall, last.mono) > TRUST_FOR_MS) stalled.abort(STALLED);
      }, STALL_LOOK_MS);
      try {
        for await (const msg of client.watchServers({}, { signal: stalled.signal })) {
          heard.wall = Date.now();
          heard.mono = performance.now();
          if (msg.heartbeat) continue;
          const next = msg.server;
          if (next && msg.removed) {
            servers.delete(next.id);
          } else if (next) {
            const prev = servers.get(next.id);
            if (synced) clock.observe(prev, next);
            servers.set(next.id, merge(prev, next));
          }
          synced ||= msg.snapshotComplete;
          if (!synced) continue;
          known = [...servers.values()];
          publish({ status: "live", servers: known }, clock.skewMs);
          connected();
        }
      } catch (err) {
        throw stalled.signal.reason === STALLED ? STALLED : err;
      } finally {
        clearInterval(looking);
        attempt.removeEventListener("abort", stop);
      }
      throw new ConnectError("the watch ended");
    },
    (err) => {
      // A session that ended is the gate's to handle: the transport has told
      // it, and it takes the page away if the session is really gone. Until
      // then the watch keeps trying, without showing an error for it.
      if (!isSessionEnded(err)) {
        publish(
          known
            ? { status: "reconnecting", servers: known, error: ConnectError.from(err) }
            : { status: "connecting", error: ConnectError.from(err) },
          clock.skewMs,
        );
      }
      return undefined;
    },
  );
}

/**
 * How far the browser's clock runs ahead of the control plane's. When the
 * control plane hears from an agent it stamps the server's last_seen_time
 * with its own clock and tells watchers at once, so a message with a newer
 * last_seen_time arrives moments after that time. The smallest of the last
 * few differences leaves out messages that waited in a queue.
 */
class SkewEstimate {
  private readonly recent: number[] = [];
  skewMs = 0;

  observe(prev: Server | undefined, next: Server): void {
    if (next.connection !== Connection.CONNECTED || !next.lastSeenTime) return;
    const seen = timestampMs(next.lastSeenTime);
    if (prev?.lastSeenTime && seen <= timestampMs(prev.lastSeenTime)) return;
    this.recent.push(Date.now() - seen);
    if (this.recent.length > SKEW_SAMPLES) this.recent.shift();
    this.skewMs = Math.min(...this.recent);
  }
}

const SKEW_SAMPLES = 12;

/**
 * Folds a new message into what is known: history arrives once, then each
 * new sample appends, as the control plane keeps it. Messages about
 * something else, such as the agent disconnecting, repeat the latest
 * sample, which is already there.
 */
function merge(prev: Server | undefined, next: Server): Server {
  if (next.history.length > 0 || !prev) return next;
  const fresh = next.stats && !sameTime(next.stats.time, prev.stats?.time);
  const history = fresh && next.stats ? appendSample(prev.history, next.stats) : prev.history;
  return { ...next, history };
}

function sameTime(a: Timestamp | undefined, b: Timestamp | undefined): boolean {
  return a !== undefined && b !== undefined && a.seconds === b.seconds && a.nanos === b.nanos;
}
