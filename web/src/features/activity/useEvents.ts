// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ActorKind, type Event, type EventKind, EventService, type WatchEventsResponse } from "@dockwell/api";
import { useCallback, useEffect, useRef, useState } from "react";

import { isSessionEnded } from "../../lib/errors";
import { keepTrying } from "../../lib/retry";
import { PAGE_SIZE, useEventsHub } from "./EventsHub";

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

const BACKOFF = { minMs: 1_000, maxMs: 15_000 };

/** How long a filtered timeline waits after an event before reading its page again, so a burst reads once. */
const READ_AGAIN_MS = 250;

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
 * The timeline, live, from the tab's one WatchEvents stream (EventsHub): its
 * snapshot of the newest page, then each new event and each change as it moves
 * on. A filtered timeline reads its own newest page with ListEvents. Older
 * pages come from ListEvents on request. A dropped stream reconnects with
 * backoff, keeps showing what it had, and merges in the fresh snapshot.
 */
export function useEvents(filter: EventFilter): Events {
  const transport = useTransport();
  const hub = useEventsHub();
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
    const show = () => {
      const status = hub.status;
      const error = status.kind === "live" ? undefined : status.error;
      setShown({
        state: {
          status: status.kind === "reconnecting" ? "reconnecting" : "live",
          events: [...loaded.current.events.values()].sort(newestFirst),
          ...(error ? { error } : {}),
        },
        older: loaded.current.older,
      });
    };
    const takeIn = (msg: Pick<WatchEventsResponse, "events" | "removed" | "snapshot" | "nextPageToken">) => {
      if (msg.snapshot) {
        const { loaded: next, kept } = takeSnapshot(started ? loaded.current : undefined, msg as WatchEventsResponse);
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
      show();
    };

    // The tab's one stream carries every event. A timeline of all of them takes it
    // as it comes; one that's filtered reads its own newest page, as the stream's
    // holds every kind, and takes from the stream what matches it from then on.
    const filtered =
      kinds.length > 0 || serverId !== "" || resourceId !== "" || actorId !== "" || actorKind !== ActorKind.UNSPECIFIED;
    // What only the control plane can match, it matches: such a timeline reads its page again as events come.
    const matchedHere = serverId === "" && resourceId === "";
    const matches = (event: Event) =>
      (kinds.length === 0 || kinds.includes(event.kind)) &&
      (actorId === "" || event.actor?.id === actorId) &&
      (actorKind === ActorKind.UNSPECIFIED || event.actor?.kind === actorKind);
    const client = createClient(EventService, transport);
    let timer: ReturnType<typeof setTimeout> | undefined;
    const readPage = () =>
      keepTrying(
        abort.signal,
        BACKOFF,
        async (connected, attempt) => {
          const page = await client.listEvents(
            { kinds, serverId, resourceId, actorId, actorKind, pageSize: PAGE_SIZE },
            { signal: attempt },
          );
          connected();
          takeIn({ events: page.events, removed: [], snapshot: true, nextPageToken: page.nextPageToken });
          return "done";
        },
        (err) => {
          // An ended session is the gate's to handle, as for the fleet.
          if (isSessionEnded(err)) return "done";
          const error = ConnectError.from(err);
          if (started) setShown((was) => ({ ...was, state: { ...was.state, error } as EventsState }));
          else setShown({ state: { status: "connecting", error }, older: "" });
          return undefined;
        },
      );
    const readSoon = () => {
      clearTimeout(timer);
      timer = setTimeout(() => void readPage(), READ_AGAIN_MS);
    };
    const unsubscribe = hub.subscribe((msg) => {
      if (!filtered) {
        takeIn(msg);
        return;
      }
      // A snapshot after the first says the stream was away: the page is read again.
      if (msg.snapshot) {
        if (started) readSoon();
        return;
      }
      if (!matchedHere) {
        if (msg.events.length > 0 || msg.removed.length > 0) readSoon();
        return;
      }
      const mine = msg.events.filter(matches);
      if (mine.length > 0 || msg.removed.length > 0) takeIn({ ...msg, events: mine });
    });
    const unwatch = hub.watch(() => {
      if (started) show();
      else if (!filtered && hub.status.kind === "connecting" && hub.status.error)
        setShown({ state: { status: "connecting", error: hub.status.error }, older: "" });
    });
    if (filtered) void readPage();
    return () => {
      abort.abort();
      clearTimeout(timer);
      unsubscribe();
      unwatch();
      stopListing();
    };
  }, [hub, transport, kindList, serverId, resourceId, actorId, actorKind, stopListing]);

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
