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
}

const START: StreamState = { status: "connecting", caughtUp: false, sources: new Map(), servers: new Map() };

/** Failures that trying again won't fix, as what was asked for stays as it is. */
export function final(err: ConnectError): boolean {
  return err.code === Code.PermissionDenied || err.code === Code.NotFound || err.code === Code.InvalidArgument;
}

/**
 * Follows a stream of logs into a store. A dropped stream carries on after the
 * last line it had, with no line twice and none missing, as far as its servers
 * still have them. Lines go in at most every FLUSH_MS, whatever the rate they
 * come at, and a reader this page can't keep up with reads more slowly, which
 * the agent feels and answers by dropping lines, and saying how many.
 *
 * Leaving stops it at once, all the way to the agent.
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

  // biome-ignore lint/correctness/useExhaustiveDependencies: key says when what is asked for changes
  useEffect(() => {
    const req = latest.current;
    if (!req) {
      setState(START);
      return undefined;
    }
    const abort = new AbortController();
    const client = createClient(LogService, transport);
    store.clear();
    setState(START);
    let token = "";
    let pending: LogChunk[] = [];
    let timer: ReturnType<typeof setTimeout> | undefined;
    let flushed = 0;
    const sources = new Map<string, LogSource>();
    const servers = new Map<string, ServerLogStatus>();
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
        for await (const m of client.watchLogs({ ...req, resumeToken: token }, { signal: attempt })) {
          connected();
          if (m.resumeToken) token = m.resumeToken;
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
            setState((was) => ({
              ...was,
              status: "live",
              caughtUp: was.caughtUp || m.caughtUp,
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
          setState((was) => ({ ...was, status: "done", caughtUp: true }));
          return "done";
        }
        throw new ConnectError("the stream of logs ended");
      },
      (err) => {
        if (isSessionEnded(err)) return "done"; // the gate's to handle
        const error = ConnectError.from(err);
        if (final(error)) {
          setState((was) => ({ ...was, status: "failed", error }));
          return "done";
        }
        setState((was) => ({ ...was, status: was.status === "connecting" ? "connecting" : "reconnecting", error }));
        return undefined;
      },
    );
    return () => {
      abort.abort();
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [transport, key, store]);

  return state;
}

function bigints(_key: string, value: unknown): unknown {
  return typeof value === "bigint" ? value.toString() : value;
}
