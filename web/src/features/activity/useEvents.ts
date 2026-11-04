// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type ActorKind, type Event, type EventKind, EventService } from "@dockwell/api";
import { useCallback, useEffect, useRef, useState } from "react";

import { isSessionEnded } from "../../lib/errors";

/** Which events to show; every field set must hold. */
export interface EventFilter {
  kinds: EventKind[];
  serverId: string;
  resourceId: string;
  actorId: string;
  actorKind: ActorKind;
}

export type EventsState =
  | { status: "connecting"; error?: ConnectError }
  | { status: "live" | "reconnecting"; events: Event[]; error?: ConnectError };

export interface Events {
  state: EventsState;
  /** Where ListEvents carries on with older events; empty once there are none. */
  older: string;
  loadingOlder: boolean;
  olderError?: ConnectError;
  /** Reads the next page of older events and adds it below. */
  loadOlder(): void;
}

const PAGE_SIZE = 50;
const MIN_BACKOFF_MS = 1_000;
const MAX_BACKOFF_MS = 15_000;

/** Newest first, then by ID, as the API orders them. */
export function newestFirst(a: Event, b: Event): number {
  const at = a.time ? timestampMs(a.time) : 0;
  const bt = b.time ? timestampMs(b.time) : 0;
  if (at !== bt) return bt - at;
  return a.id < b.id ? 1 : a.id > b.id ? -1 : 0;
}

/**
 * The timeline, live, from EventService.WatchEvents: its snapshot of the
 * newest page, then each new event and each change as it moves on. Older
 * pages come from ListEvents on request. A dropped watch reconnects with
 * backoff, keeps showing what it had, and swaps in the fresh snapshot.
 */
export function useEvents(filter: EventFilter): Events {
  const transport = useTransport();
  const [state, setState] = useState<EventsState>({ status: "connecting" });
  const [older, setOlder] = useState("");
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [olderError, setOlderError] = useState<ConnectError>();
  // Events from older pages, kept apart so a fresh snapshot doesn't drop them.
  const paged = useRef(new Map<string, Event>());
  const { serverId, resourceId, actorId, actorKind } = filter;
  // The kinds as one value, so a new array of the same kinds doesn't watch again.
  const kindList = filter.kinds.join(",");

  useEffect(() => {
    const kinds = kindsOf(kindList);
    const abort = new AbortController();
    paged.current = new Map();
    setState({ status: "connecting" });
    setOlder("");
    setOlderError(undefined);
    const request = { kinds, serverId, resourceId, actorId, actorKind, pageSize: PAGE_SIZE };
    void watch(transport, request, abort.signal, (events, next, status, error, removed) => {
      if (next !== undefined) setOlder(next);
      for (const id of removed ?? []) paged.current.delete(id);
      setState(
        events
          ? { status, events: merged(events, paged.current), ...(error ? { error } : {}) }
          : { status: "connecting", ...(error ? { error } : {}) },
      );
    });
    return () => abort.abort();
  }, [transport, kindList, serverId, resourceId, actorId, actorKind]);

  const loadOlder = useCallback(() => {
    if (!older || loadingOlder) return;
    setLoadingOlder(true);
    setOlderError(undefined);
    const client = createClient(EventService, transport);
    const kinds = kindsOf(kindList);
    client
      .listEvents({ kinds, serverId, resourceId, actorId, actorKind, pageSize: PAGE_SIZE, pageToken: older })
      .then((res) => {
        for (const event of res.events) paged.current.set(event.id, event);
        setOlder(res.nextPageToken);
        setState((was) => (was.status === "connecting" ? was : { ...was, events: merged(was.events, paged.current) }));
      })
      .catch((err: unknown) => setOlderError(ConnectError.from(err)))
      .finally(() => setLoadingOlder(false));
  }, [older, loadingOlder, transport, kindList, serverId, resourceId, actorId, actorKind]);

  return { state, older, loadingOlder, ...(olderError ? { olderError } : {}), loadOlder };
}

function kindsOf(list: string): EventKind[] {
  return list === "" ? [] : list.split(",").map((k) => Number(k) as EventKind);
}

/** The live events with the older pages below them, each event once, the live one winning. */
function merged(live: readonly Event[], paged: Map<string, Event>): Event[] {
  const byId = new Map<string, Event>();
  for (const event of paged.values()) byId.set(event.id, event);
  for (const event of live) byId.set(event.id, event);
  return [...byId.values()].sort(newestFirst);
}

type Publish = (
  events: Event[] | undefined,
  next: string | undefined,
  status: "live" | "reconnecting",
  error?: ConnectError,
  removed?: readonly string[],
) => void;

async function watch(
  transport: Transport,
  request: Parameters<ReturnType<typeof createClient<typeof EventService>>["watchEvents"]>[0],
  signal: AbortSignal,
  publish: Publish,
): Promise<void> {
  const client = createClient(EventService, transport);
  let backoff = MIN_BACKOFF_MS;
  let known: Event[] | undefined;
  while (!signal.aborted) {
    try {
      const events = new Map<string, Event>();
      for await (const msg of client.watchEvents(request, { signal })) {
        if (msg.snapshot) events.clear();
        for (const event of msg.events) events.set(event.id, event);
        for (const id of msg.removed) events.delete(id);
        known = [...events.values()].sort(newestFirst);
        publish(known, msg.snapshot ? msg.nextPageToken : undefined, "live", undefined, msg.removed);
        backoff = MIN_BACKOFF_MS;
      }
      throw new ConnectError("the watch ended");
    } catch (err) {
      if (signal.aborted) return;
      // An ended session is the gate's to handle, as for the fleet.
      if (!isSessionEnded(err)) publish(known, undefined, "reconnecting", ConnectError.from(err));
      await delay(backoff + Math.random() * backoff, signal);
      backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
    }
  }
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
