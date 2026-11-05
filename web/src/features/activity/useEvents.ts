// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type ActorKind, type Event, type EventKind, EventService, type WatchEventsResponse } from "@dockwell/api";
import { useCallback, useEffect, useRef, useState } from "react";

import { isSessionEnded } from "../../lib/errors";
import { keepTrying } from "../../lib/retry";

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
const BACKOFF = { minMs: 1_000, maxMs: 15_000 };

/** Newest first, then by ID, as the API orders them. */
export function newestFirst(a: Event, b: Event): number {
  const at = a.time ? timestampMs(a.time) : 0;
  const bt = b.time ? timestampMs(b.time) : 0;
  if (at !== bt) return bt - at;
  return a.id < b.id ? 1 : a.id > b.id ? -1 : 0;
}

/** What is loaded: each event shown, by ID, and where ListEvents carries on below them. */
export interface Loaded {
  events: Map<string, Event>;
  /** Empty once there is nothing older. */
  older: string;
}

/**
 * Takes a watch's snapshot into what is loaded, and says whether the
 * pages loaded before carry on below it. The first snapshot is all there
 * is. After a reconnect, the snapshot holds the newest events, which may
 * have moved on or left since; what was loaded below it stays, with
 * paging carrying on where it was, as long as the two meet, so nothing
 * that happened in between is missing. If more happened while the watch
 * was away than the snapshot holds, there would be a gap, so the snapshot
 * starts over and paging carries on below it.
 */
export function takeSnapshot(
  was: Loaded | undefined,
  snapshot: WatchEventsResponse,
): { loaded: Loaded; kept: boolean } {
  const fresh = new Map(snapshot.events.map((e) => [e.id, e] as const));
  const startOver = { loaded: { events: fresh, older: snapshot.nextPageToken }, kept: false };
  // A snapshot without a next page holds every event there is.
  if (!was || was.events.size === 0 || snapshot.nextPageToken === "") return startOver;
  const oldest = [...fresh.values()].sort(newestFirst).at(-1);
  const newest = [...was.events.values()].sort(newestFirst)[0];
  if (!oldest || !newest || newestFirst(newest, oldest) > 0) return startOver;
  for (const event of was.events.values()) {
    if (!fresh.has(event.id) && newestFirst(event, oldest) > 0) fresh.set(event.id, event);
  }
  return { loaded: { events: fresh, older: was.older }, kept: true };
}

/**
 * The timeline, live, from EventService.WatchEvents: its snapshot of the
 * newest page, then each new event and each change as it moves on. Older
 * pages come from ListEvents on request. A dropped watch reconnects with
 * backoff, keeps showing what it had, and merges in the fresh snapshot.
 */
export function useEvents(filter: EventFilter): Events {
  const transport = useTransport();
  const [shown, setShown] = useState<{ state: EventsState; older: string }>({
    state: { status: "connecting" },
    older: "",
  });
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [olderError, setOlderError] = useState<ConnectError>();
  // What this filter's watch has loaded. A new filter, or a reconnect that
  // starts over, puts a new one here, so a page asked for before lands
  // nowhere.
  const loaded = useRef<Loaded>({ events: new Map(), older: "" });
  // The older page being read, if one is.
  const listing = useRef<AbortController | undefined>(undefined);
  const { serverId, resourceId, actorId, actorKind } = filter;
  // The kinds as one value, so a new array of the same kinds doesn't watch again.
  const kindList = filter.kinds.join(",");

  const stopListing = useCallback(() => {
    listing.current?.abort();
    listing.current = undefined;
    setLoadingOlder(false);
  }, []);

  useEffect(() => {
    const kinds = kindsOf(kindList);
    const abort = new AbortController();
    stopListing();
    loaded.current = { events: new Map(), older: "" };
    setShown({ state: { status: "connecting" }, older: "" });
    setOlderError(undefined);
    let started = false;
    const show = (status: "live" | "reconnecting", error?: ConnectError) =>
      setShown({
        state: { status, events: [...loaded.current.events.values()].sort(newestFirst), ...(error ? { error } : {}) },
        older: loaded.current.older,
      });
    const request = { kinds, serverId, resourceId, actorId, actorKind, pageSize: PAGE_SIZE };
    const client = createClient(EventService, transport);
    void keepTrying(
      abort.signal,
      BACKOFF,
      async (connected, attempt) => {
        for await (const msg of client.watchEvents(request, { signal: attempt })) {
          if (msg.snapshot) {
            const { loaded: next, kept } = takeSnapshot(started ? loaded.current : undefined, msg);
            if (kept) {
              loaded.current.events = next.events;
            } else {
              stopListing();
              loaded.current = next;
            }
            started = true;
          } else {
            for (const event of msg.events) loaded.current.events.set(event.id, event);
            for (const id of msg.removed) loaded.current.events.delete(id);
          }
          show("live");
          connected();
        }
        throw new ConnectError("the watch ended");
      },
      (err) => {
        // An ended session is the gate's to handle, as for the fleet.
        if (isSessionEnded(err)) return undefined;
        const error = ConnectError.from(err);
        if (started) show("reconnecting", error);
        else setShown({ state: { status: "connecting", error }, older: "" });
        return undefined;
      },
    );
    return () => {
      abort.abort();
      stopListing();
    };
  }, [transport, kindList, serverId, resourceId, actorId, actorKind, stopListing]);

  const loadOlder = useCallback(() => {
    const into = loaded.current;
    if (!into.older || listing.current) return;
    const abort = new AbortController();
    listing.current = abort;
    setLoadingOlder(true);
    setOlderError(undefined);
    createClient(EventService, transport)
      .listEvents(
        {
          kinds: kindsOf(kindList),
          serverId,
          resourceId,
          actorId,
          actorKind,
          pageSize: PAGE_SIZE,
          pageToken: into.older,
        },
        { signal: abort.signal },
      )
      .then((res) => {
        // Asked for under another filter, or before the watch started over.
        if (loaded.current !== into || abort.signal.aborted) return;
        // What the watch has is newer than any page.
        for (const event of res.events) if (!into.events.has(event.id)) into.events.set(event.id, event);
        into.older = res.nextPageToken;
        setShown((was) =>
          was.state.status === "connecting"
            ? was
            : { state: { ...was.state, events: [...into.events.values()].sort(newestFirst) }, older: into.older },
        );
      })
      .catch((err: unknown) => {
        if (loaded.current === into && !abort.signal.aborted) setOlderError(ConnectError.from(err));
      })
      .finally(() => {
        if (listing.current === abort) {
          listing.current = undefined;
          setLoadingOlder(false);
        }
      });
  }, [transport, kindList, serverId, resourceId, actorId, actorKind]);

  return {
    state: shown.state,
    older: shown.older,
    loadingOlder,
    ...(olderError ? { olderError } : {}),
    loadOlder,
  };
}

function kindsOf(list: string): EventKind[] {
  return list === "" ? [] : list.split(",").map((k) => Number(k) as EventKind);
}
