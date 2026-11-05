// SPDX-License-Identifier: AGPL-3.0-only

import { ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type LogLevel, LogService, type LogSource, type ServerLogStatus } from "@dockwell/api";
import { useCallback, useEffect, useRef, useState } from "react";

import type { LogStore } from "./store";

export interface DeepSearch {
  loading: boolean;
  /** The oldest line it read, in milliseconds. Zero when it read none. */
  reached: number;
  lines: bigint;
  cutShort: boolean;
  /** It stopped as its time ran out, rather than at the most lines it reads. */
  timedOut: boolean;
  next: string;
  error?: ConnectError | undefined;
  sources: readonly LogSource[];
  /** The servers that couldn't be read, whose lines the answer doesn't have. */
  servers: readonly ServerLogStatus[];
  loadMore(): void;
  retry(): void;
}

type State = Omit<DeepSearch, "loadMore" | "retry">;

const START: State = {
  loading: false,
  reached: 0,
  lines: 0n,
  cutShort: false,
  timedOut: false,
  next: "",
  sources: [],
  servers: [],
};

/** Searches all the servers keep, through ReadLogs: what Docker has of each container, and what they kept. */
export function useDeepSearch(
  serviceId: string,
  on: boolean,
  query: string,
  levels: readonly LogLevel[],
  store: LogStore,
): DeepSearch {
  const transport = useTransport();
  const [state, setState] = useState<State>(START);
  const [attempt, setAttempt] = useState(0);
  const levelList = levels.join(",");
  const run = useRef<(token: string, keep: boolean) => void>(() => {});
  // biome-ignore lint/correctness/useExhaustiveDependencies: attempt says to search again
  useEffect(() => {
    if (!on) return undefined;
    const abort = new AbortController();
    const client = createClient(LogService, transport);
    const wanted = levelList === "" ? [] : (levelList.split(",").map(Number) as LogLevel[]);
    const read = (pageToken: string, keep: boolean) => {
      if (!keep) store.clear();
      setState((was) => ({ ...was, loading: true, error: undefined }));
      void client
        .readLogs({ id: serviceId, query, levels: wanted, pageSize: 5000, pageToken }, { signal: abort.signal })
        .then((res) => {
          store.addOlder(res.chunks);
          const t = res.searchedBackTo;
          setState((was) => ({
            ...was,
            loading: false,
            reached: t ? Number(t.seconds) * 1000 : was.reached,
            lines: was.lines + res.searchedLines,
            cutShort: res.searchCutShort,
            timedOut: res.searchTimedOut,
            next: res.nextPageToken,
            sources: [...was.sources, ...res.sources],
            servers: res.servers,
          }));
        })
        .catch((err: unknown) => {
          if (!abort.signal.aborted) setState((was) => ({ ...was, loading: false, error: ConnectError.from(err) }));
        });
    };
    run.current = read;
    setState({ ...START, loading: true });
    read("", false);
    return () => abort.abort();
  }, [on, serviceId, query, levelList, store, transport, attempt]);
  const next = state.next;
  const loadMore = useCallback(() => run.current(next, true), [next]);
  const retry = useCallback(() => setAttempt((n) => n + 1), []);
  return { ...state, loadMore, retry };
}
