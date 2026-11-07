// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type LogChunk,
  LogService,
  type LogSource,
  type ServerLogStatus,
  type WatchLogsRequestSchema,
} from "@dockwell/api";
import { useEffect, useRef, useState } from "react";

import { isSessionEnded } from "../../lib/errors";
import { useAway } from "../../lib/outOfSight";
import { keepTrying } from "../../lib/retry";
import type { LogStore } from "./store";

/** How often lines go into the view, at most, so a flood redraws at this rate and no faster. */
export const FLUSH_MS = 100;

const BACKOFF = { minMs: 500, maxMs: 15_000 };

type Request = MessageInitShape<typeof WatchLogsRequestSchema>;

export interface StreamState {
  /** connecting: nothing yet. live: following. done: read to its end. */
  status: "connecting" | "live" | "done" | "reconnecting" | "failed";
  /** Every server reachable has sent what it had, so no lines means none. */
  caughtUp: boolean;
  sources: ReadonlyMap<string, LogSource>;
  servers: ReadonlyMap<string, ServerLogStatus>;
  error?: ConnectError;
  /** Back from a tab out of sight, it's reading what came meanwhile, and isn't live until it has. */
  catchingUp?: boolean;
  /** Back from a tab out of sight for long, it started again from the newest lines: what came then wasn't read. */
  missed?: { from: number; to: number };
}

/**
 * How long a tab can be out of sight and still read all that came meanwhile on
 * coming back. Past it, reading hours of lines through the tunnel only to keep the
 * newest would take longer than it's worth, so it starts again from the newest
 * and says what it didn't read.
 */
export const LONG_AWAY_MS = 10 * 60_000;

const START: StreamState = { status: "connecting", caughtUp: false, sources: new Map(), servers: new Map() };

/** Failures that trying again won't fix, as what was asked for stays as it is. */
export function final(err: ConnectError): boolean {
  return err.code === Code.PermissionDenied || err.code === Code.NotFound || err.code === Code.InvalidArgument;
}

/** What one request has read so far, kept while its stream is closed for a tab out of sight. */
interface Progress {
  token: string;
  sources: Map<string, LogSource>;
  servers: Map<string, ServerLogStatus>;
  /** Read to its end, or failed for good: nothing to carry on with. */
  finished: boolean;
  /** When its stream last closed, to know how long it was away. */
  closedAt?: number;
}

const fresh = (): Progress => ({ token: "", sources: new Map(), servers: new Map(), finished: false });

/**
 * Follows a stream of logs into a store. A dropped stream carries on after the
 * last line it had, with no line twice and none missing, as far as its servers
 * still have them. Lines go in at most every FLUSH_MS, whatever the rate they
 * come at, and a reader this page can't keep up with reads more slowly, which
 * the agent feels and answers by dropping lines, and saying how many.
 *
 * Leaving stops it at once, all the way to the agent. So does a tab out of
 * sight for a while, to free its connection for the tabs in view; it carries
 * on after its last line as the tab comes back, as after any drop.
 */
export function useLogStream(
  request: Request | undefined,
  store: LogStore,
  onSources?: () => void,
  /** Changes to read the same request again, as refreshing a time range does. */
  again = 0,
): StreamState {
  const transport = useTransport();
  const [state, setState] = useState<StreamState>(START);
  const key = request ? `${again}/${JSON.stringify(request, bigints)}` : "";
  const latest = useRef(request);
  latest.current = request;
  const heard = useRef(onSources);
  heard.current = onSources;
  const away = useAway();
  const progress = useRef<Progress>(fresh());

  // A new request starts over. This runs before the stream's own effect, after its cleanup has put its last lines in.
  // biome-ignore lint/correctness/useExhaustiveDependencies: key says when what is asked for changes
  useEffect(() => {
    progress.current = fresh();
    if (latest.current) store.clear();
    setState(START);
  }, [key, store]);

  // biome-ignore lint/correctness/useExhaustiveDependencies: key says when what is asked for changes
  useEffect(() => {
    const req = latest.current;
    const read = progress.current;
    if (!req || away || read.finished) return undefined;
    const abort = new AbortController();
    const client = createClient(LogService, transport);
    const { sources, servers } = read;
    // Back from a tab out of sight: read what came meanwhile, or, after long, start again.
    const back = read.closedAt;
    read.closedAt = undefined;
    let catching = false;
    if (back !== undefined && !req.once) {
      const now = Date.now();
      if (now - back > LONG_AWAY_MS) {
        read.token = "";
        store.markAway(back, now);
        setState((was) => ({ ...was, missed: { from: back, to: now } }));
      } else if (read.token !== "") {
        catching = true;
        setState((was) => ({ ...was, catchingUp: true }));
      }
    }
    let pending: LogChunk[] = [];
    let timer: ReturnType<typeof setTimeout> | undefined;
    let flushed = 0;
    const flush = () => {
      timer = undefined;
      flushed = performance.now();
      const batch = pending;
      pending = [];
      store.add(batch);
    };
    const schedule = () => {
      if (timer !== undefined) return;
      timer = setTimeout(flush, Math.max(0, FLUSH_MS - (performance.now() - flushed)));
    };
    void keepTrying(
      abort.signal,
      BACKOFF,
      async (connected, attempt) => {
        // Once for each connection, as React keeps every update until it next
        // renders, and one that changes nothing doesn't make it, so a long follow
        // would keep them all, with what each message's closure holds.
        let saidLive = false;
        for await (const m of client.watchLogs({ ...req, resumeToken: read.token }, { signal: attempt })) {
          connected();
          if (m.resumeToken) read.token = m.resumeToken;
          let changed = false;
          for (const s of m.sources) {
            sources.set(s.id, s);
            changed = true;
          }
          for (const s of m.servers) {
            servers.set(s.serverId, s);
            changed = true;
          }
          if (changed) heard.current?.();
          if (m.chunks.length > 0) {
            pending.push(...m.chunks);
            schedule();
          }
          if (changed || m.caughtUp) {
            saidLive = true;
            if (m.caughtUp) catching = false;
            setState((was) => ({
              ...was,
              status: "live",
              caughtUp: was.caughtUp || m.caughtUp,
              catchingUp: catching,
              sources: new Map(sources),
              servers: new Map(servers),
              error: undefined,
            }));
          } else if (!saidLive) {
            saidLive = true;
            setState((was) => (was.status === "live" ? was : { ...was, status: "live", error: undefined }));
          }
        }
        if (req.once) {
          if (timer !== undefined) clearTimeout(timer);
          flush();
          read.finished = true;
          setState((was) => ({ ...was, status: "done", caughtUp: true }));
          return "done";
        }
        throw new ConnectError("the stream of logs ended");
      },
      (err) => {
        if (isSessionEnded(err)) return "done"; // the gate's to handle
        const error = ConnectError.from(err);
        if (final(error)) {
          read.finished = true;
          setState((was) => ({ ...was, status: "failed", error }));
          return "done";
        }
        setState((was) => ({ ...was, status: was.status === "connecting" ? "connecting" : "reconnecting", error }));
        return undefined;
      },
    );
    return () => {
      abort.abort();
      read.closedAt = Date.now();
      // Lines heard and not yet shown go in, so a stream closed for a tab out of sight carries on without a gap.
      if (timer !== undefined) {
        clearTimeout(timer);
        flush();
      }
    };
  }, [transport, key, store, away]);

  return state;
}

function bigints(_key: string, value: unknown): unknown {
  return typeof value === "bigint" ? value.toString() : value;
}
