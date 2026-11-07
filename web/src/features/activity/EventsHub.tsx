// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Event, EventService, type WatchEventsResponse, WatchEventsResponseSchema } from "@dockwell/api";
import { createContext, type ReactNode, useContext, useEffect, useState } from "react";

import { isSessionEnded } from "../../lib/errors";
import { useAway } from "../../lib/outOfSight";
import { keepTrying } from "../../lib/retry";

/** How many of the newest events the stream's snapshot holds. */
export const PAGE_SIZE = 50;

const BACKOFF = { minMs: 1_000, maxMs: 15_000 };

export type HubStatus =
  | { kind: "connecting"; error?: ConnectError }
  | { kind: "live" }
  | { kind: "reconnecting"; error?: ConnectError };

type Listener = (msg: WatchEventsResponse) => void;

/** Newest first, then by ID, as the API orders them. */
function newestFirst(a: Event, b: Event): number {
  const at = a.time ? timestampMs(a.time) : 0;
  const bt = b.time ? timestampMs(b.time) : 0;
  if (at !== bt) return bt - at;
  return a.id < b.id ? 1 : a.id > b.id ? -1 : 0;
}

/**
 * One WatchEvents stream for a tab, which every screen's timeline and the
 * services' re-read share. Over plain HTTP, as through the SSH tunnel, a
 * browser keeps six connections to the panel for all its tabs, and each open
 * stream holds one; a stream per consumer let two tabs take them all, and a
 * read waited with nothing to answer it.
 *
 * It keeps the newest page as last heard, so a screen that opens later starts
 * from it, and hands each message on as it comes. A new snapshot after the
 * first says the stream was away, and what it holds now.
 */
export class EventsHub {
  private listeners = new Set<Listener>();
  private watchers = new Set<() => void>();
  private abort: AbortController | undefined;
  private events = new Map<string, Event>();
  private older = "";
  private heard = false;
  status: HubStatus = { kind: "connecting" };

  constructor(private readonly transport: Transport) {}

  /** Opens the stream, unless it's open. */
  open(): void {
    if (this.abort) return;
    const abort = new AbortController();
    this.abort = abort;
    const client = createClient(EventService, this.transport);
    void keepTrying(
      abort.signal,
      BACKOFF,
      async (connected, attempt) => {
        for await (const msg of client.watchEvents({ pageSize: PAGE_SIZE }, { signal: attempt })) {
          if (msg.snapshot) {
            this.events = new Map(msg.events.map((e) => [e.id, e] as const));
            this.older = msg.nextPageToken;
            this.heard = true;
          } else {
            for (const event of msg.events) this.events.set(event.id, event);
            for (const id of msg.removed) this.events.delete(id);
          }
          connected();
          this.setStatus({ kind: "live" });
          for (const listener of this.listeners) listener(msg);
        }
        throw new ConnectError("the watch ended");
      },
      (err) => {
        // An ended session is the gate's to handle.
        if (isSessionEnded(err)) return "done";
        const error = ConnectError.from(err);
        this.setStatus(this.heard ? { kind: "reconnecting", error } : { kind: "connecting", error });
        return undefined;
      },
    );
  }

  /** Closes the stream, as for a tab out of sight. Opening again starts with a snapshot. */
  close(): void {
    this.abort?.abort();
    this.abort = undefined;
  }

  /**
   * Hands every message on from now, after a snapshot of the newest page as it
   * stands, if the stream has heard one: a screen that opens later starts there.
   */
  subscribe(listener: Listener): () => void {
    this.listeners.add(listener);
    if (this.heard) {
      listener(
        create(WatchEventsResponseSchema, {
          events: [...this.events.values()].sort(newestFirst),
          snapshot: true,
          nextPageToken: this.older,
        }),
      );
    }
    return () => this.listeners.delete(listener);
  }

  /** Says when status changes, for useSyncExternalStore. */
  watch(onChange: () => void): () => void {
    this.watchers.add(onChange);
    return () => this.watchers.delete(onChange);
  }

  private setStatus(next: HubStatus): void {
    if (this.status.kind === "live" && next.kind === "live") return;
    this.status = next;
    for (const watcher of this.watchers) watcher();
  }
}

const Hub = createContext<EventsHub | undefined>(undefined);

/**
 * The tab's events stream, for the shell to hold. It closes once the tab has
 * been out of sight for a while, and opens again as it comes back, when what
 * listens reads again as after any reconnect.
 */
export function EventsHubProvider({ children }: { children: ReactNode }) {
  const transport = useTransport();
  const [hub] = useState(() => new EventsHub(transport));
  const away = useAway();
  useEffect(() => {
    if (away) return undefined;
    hub.open();
    return () => hub.close();
  }, [hub, away]);
  return <Hub.Provider value={hub}>{children}</Hub.Provider>;
}

/**
 * The shell's events stream, or, outside the shell, one of this component's
 * own, so a screen works wherever it's drawn.
 */
export function useEventsHub(): EventsHub {
  const shared = useContext(Hub);
  const transport = useTransport();
  const [own] = useState(() => (shared ? undefined : new EventsHub(transport)));
  useEffect(() => {
    if (!own) return;
    own.open();
    return () => own.close();
  }, [own]);
  return shared ?? (own as EventsHub);
}
