// SPDX-License-Identifier: AGPL-3.0-only

import { ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Server, ServerService } from "@dockwell/api";
import { useEffect, useState } from "react";

/** How many samples each server keeps for sparklines, matching the control plane. */
export const HISTORY_LENGTH = 60;

export type FleetState =
  | { status: "connecting"; error?: ConnectError }
  | { status: "live"; servers: Server[] }
  | { status: "reconnecting"; servers: Server[]; error: ConnectError };

const MIN_BACKOFF_MS = 1_000;
const MAX_BACKOFF_MS = 15_000;

/**
 * Every server and its live stats, from ServerService.WatchServers. The
 * stream opens with a snapshot, each server with its history, and marks its
 * end; later messages carry only the newest sample, appended here. A dropped
 * stream reconnects with backoff, keeps showing what it knew, and swaps in
 * the fresh snapshot once it is complete.
 */
export function useFleet(): FleetState {
  const transport = useTransport();
  const [state, setState] = useState<FleetState>({ status: "connecting" });

  useEffect(() => {
    const abort = new AbortController();
    void watch(transport, abort.signal, setState);
    return () => abort.abort();
  }, [transport]);

  return state;
}

async function watch(transport: Transport, signal: AbortSignal, publish: (s: FleetState) => void): Promise<void> {
  const client = createClient(ServerService, transport);
  let backoff = MIN_BACKOFF_MS;
  let known: Server[] | undefined;
  while (!signal.aborted) {
    const servers = new Map<string, Server>();
    let synced = false;
    try {
      for await (const msg of client.watchServers({}, { signal })) {
        const next = msg.server;
        if (next && msg.removed) {
          servers.delete(next.id);
        } else if (next) {
          servers.set(next.id, merge(servers.get(next.id), next));
        }
        synced ||= msg.snapshotComplete;
        if (!synced) continue;
        known = [...servers.values()];
        publish({ status: "live", servers: known });
        backoff = MIN_BACKOFF_MS;
      }
      throw new ConnectError("the watch ended");
    } catch (err) {
      if (signal.aborted) return;
      publish(
        known
          ? { status: "reconnecting", servers: known, error: ConnectError.from(err) }
          : { status: "connecting", error: ConnectError.from(err) },
      );
      await delay(backoff + Math.random() * backoff, signal);
      backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
    }
  }
}

/** Folds a new message into what is known: history arrives once, then samples append. */
function merge(prev: Server | undefined, next: Server): Server {
  if (next.history.length > 0 || !prev) return next;
  const history = next.stats ? [...prev.history, next.stats].slice(-HISTORY_LENGTH) : prev.history;
  return { ...next, history };
}

function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });
}
