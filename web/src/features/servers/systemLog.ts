// SPDX-License-Identifier: AGPL-3.0-only

import { timestampFromMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { type ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { LogLevel, ServerService, type SystemLogLine, SystemLogSource } from "@dockwell/api";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { isSessionEnded, reasonOf } from "../../lib/errors";
import { keepTrying } from "../../lib/retry";

/** A server's system log, as its URL says what to show (ADR 0067). */
export interface SystemLogSearch {
  /** Only what this system service wrote. */
  unit?: string;
  /** Only errors, or warnings and up. */
  level?: "error" | "warn";
  /** A time range back from now; none follows live. */
  range?: SystemLogRange;
  /** Only lines that hold this. */
  q?: string;
}

/** The time ranges, as the URL names them. */
export type SystemLogRange = "15m" | "1h" | "24h" | "7d";

export const RANGE_MS: Readonly<Record<SystemLogRange, number>> = {
  "15m": 15 * 60_000,
  "1h": 3_600_000,
  "24h": 86_400_000,
  "7d": 7 * 86_400_000,
};

const RANGES: readonly SystemLogRange[] = ["15m", "1h", "24h", "7d"];

/** The longest search, as the server takes. */
export const MAX_QUERY = 1024;

/** Reads what the URL says, leaving out what isn't one of its values. */
export function readSystemLogSearch(raw: Record<string, unknown>): SystemLogSearch {
  const out: SystemLogSearch = {};
  const text = (k: string) => (typeof raw[k] === "string" ? (raw[k] as string) : undefined);
  const unit = text("unit");
  if (unit && /^[A-Za-z0-9:_.@\\-]+$/.test(unit) && unit.length <= 256) out.unit = unit;
  const level = text("level");
  if (level === "error" || level === "warn") out.level = level;
  const range = text("range");
  if (range && (RANGES as readonly string[]).includes(range)) out.range = range as SystemLogRange;
  const q = text("q");
  if (q && q.length <= MAX_QUERY) out.q = q;
  return out;
}

const LEVEL = { error: LogLevel.ERROR, warn: LogLevel.WARN } as const;

/** One entry of what the page shows: a line, or a note in its place. */
export type SystemLogEntry =
  | { kind: "line"; line: SystemLogLine }
  | { kind: "dropped"; count: number; at: number; key: string }
  | { kind: "gap"; at: number; key: string };

/** What the page holds of a server's system log. */
export interface SystemLogState {
  /** Oldest first. */
  entries: SystemLogEntry[];
  /** Whether it has read anything yet. */
  read: boolean;
  /** Where the log is kept. */
  source: SystemLogSource;
  /** Older lines to read. */
  more: boolean;
  /** For a search that stopped at its bound: how far back it looked, in milliseconds. */
  searchedTo: number | undefined;
  /** True while it follows live. */
  following: boolean;
  /** Why the last read or follow failed, until one works again. */
  error: ConnectError | undefined;
  /** True while an older page is read. */
  readingOlder: boolean;
}

/** How many lines a page reads. */
export const PAGE = 100;

/** The most lines the page keeps, the oldest going first, so a long follow stays light. */
export const MAX_LINES = 5_000;

const BACKOFF = { minMs: 1_000, maxMs: 30_000 };

const cursorOf = (e: SystemLogEntry | undefined) => (e?.kind === "line" ? e.line.cursor : "");

/**
 * A server's system log (ADR 0067): the newest page, then, live, each line as the
 * server's one follow of its journal reads it; or a time range, or a search, read
 * back a page at a time. A follow that drops reconnects after the last line it has.
 */
export function useSystemLog(serverId: string, search: SystemLogSearch, enabled: boolean) {
  const transport = useTransport();
  const client = useMemo(() => createClient(ServerService, transport), [transport]);
  const [state, setState] = useState<SystemLogState>({
    entries: [],
    read: false,
    source: SystemLogSource.UNSPECIFIED,
    more: false,
    searchedTo: undefined,
    following: false,
    error: undefined,
    readingOlder: false,
  });
  const live = !search.range && !search.q;
  const ask = useMemo(
    () => ({
      serverId,
      unit: search.unit ?? "",
      level: search.level ? LEVEL[search.level] : LogLevel.UNSPECIFIED,
      query: search.q ?? "",
      since: search.range ? timestampFromMs(Date.now() - RANGE_MS[search.range]) : undefined,
    }),
    [serverId, search.unit, search.level, search.q, search.range],
  );
  const latest = useRef(state);
  latest.current = state;

  useEffect(() => {
    if (!enabled) return;
    const abort = new AbortController();
    setState((s) => ({
      ...s,
      entries: [],
      read: false,
      more: false,
      searchedTo: undefined,
      error: undefined,
      following: false,
    }));
    const keep = (entries: SystemLogEntry[]) => (entries.length > MAX_LINES ? entries.slice(-MAX_LINES) : entries);
    void (async () => {
      let after = "";
      try {
        const res = await client.readSystemLog({ ...ask, limit: PAGE }, { signal: abort.signal });
        if (abort.signal.aborted) return;
        const lines: SystemLogEntry[] = [...res.lines].reverse().map((line) => ({ kind: "line", line }));
        after = res.lines[0]?.cursor ?? "";
        setState((s) => ({
          ...s,
          entries: lines,
          read: true,
          source: res.source,
          more: res.more,
          searchedTo: res.searchedTo ? timestampMs(res.searchedTo) : undefined,
          error: undefined,
        }));
      } catch (err) {
        if (!abort.signal.aborted) setState((s) => ({ ...s, read: true, error: err as ConnectError }));
        return;
      }
      if (!live) return;
      let notes = 0;
      await keepTrying(
        abort.signal,
        BACKOFF,
        async (connected, signal) => {
          // Carries on after the last line it has, so a reconnect misses nothing it can read back.
          const from = cursorOf(latest.current.entries.findLast((e) => e.kind === "line")) || after;
          for await (const res of client.watchSystemLog({ ...ask, after: from }, { signal })) {
            connected();
            if (res.lines.length === 0 && res.dropped === 0n && !res.gap) {
              setState((s) => (s.following && !s.error ? s : { ...s, following: true, error: undefined }));
              continue;
            }
            const at = Date.now();
            setState((s) => {
              const next: SystemLogEntry[] = [...s.entries];
              if (res.gap) next.push({ kind: "gap", at, key: `gap-${notes++}` });
              for (const line of res.lines) next.push({ kind: "line", line });
              if (res.dropped > 0n)
                next.push({ kind: "dropped", count: Number(res.dropped), at, key: `drop-${notes++}` });
              return { ...s, entries: keep(next), following: true, error: undefined };
            });
          }
          return undefined;
        },
        (err) => {
          setState((s) => ({ ...s, following: false, error: err as ConnectError }));
          const reason = reasonOf(err);
          return isSessionEnded(err) || reason === "agent_outdated" || reason === "needs_role" ? "done" : undefined;
        },
      );
    })();
    return () => abort.abort();
  }, [client, ask, live, enabled]);

  /** Reads the page before the oldest line held, or for a search, on from where it stopped. */
  const readOlder = useCallback(async () => {
    const s = latest.current;
    const oldest = s.entries.find((e) => e.kind === "line");
    if (!oldest || s.readingOlder) return;
    setState((x) => ({ ...x, readingOlder: true }));
    try {
      const res = await client.readSystemLog({
        ...ask,
        limit: PAGE,
        ...(s.searchedTo !== undefined ? { until: timestampFromMs(s.searchedTo) } : { before: cursorOf(oldest) }),
      });
      const older: SystemLogEntry[] = [...res.lines].reverse().map((line) => ({ kind: "line", line }));
      setState((x) => ({
        ...x,
        entries: [...older, ...x.entries],
        more: res.more,
        searchedTo: res.searchedTo ? timestampMs(res.searchedTo) : undefined,
        readingOlder: false,
        error: undefined,
      }));
    } catch (err) {
      setState((x) => ({ ...x, readingOlder: false, error: err as ConnectError }));
    }
  }, [client, ask]);

  return { ...state, live, readOlder };
}
