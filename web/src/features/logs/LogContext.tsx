// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { type Timestamp, TimestampSchema, timestampMs } from "@bufbuild/protobuf/wkt";
import { ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type LogChunk, LogService, type LogSource } from "@dockwell/api";
import { Callout, LogStream, type LogStreamHandle, Sheet } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";

import { sentence } from "../../lib/errors";
import { drawEntry } from "./draw";
import { type Entry, LogStore } from "./store";
import { replicaLabel, selectionText } from "./words";

/** How far around a match to read, and how many lines at most, as a reader wants what led to it. */
const BEFORE_MS = 60_000;
const AFTER_MS = 5_000;
const BEFORE_LINES = 400;
const AFTER_LINES = 200;
/** How many times to narrow what comes after, when the seconds after hold more than a page. */
const NARROWINGS = 4;

/** A time to the nanosecond, from milliseconds and the nanoseconds within their second, moved on by some. */
function timeOf(ms: number, nanos: number, plus = 0): Timestamp {
  const total = BigInt(Math.floor(ms / 1000)) * 1_000_000_000n + BigInt(nanos) + BigInt(plus);
  return create(TimestampSchema, { seconds: total / 1_000_000_000n, nanos: Number(total % 1_000_000_000n) });
}

/** How many lines chunks hold, and the time of the oldest, which a page of them reaches back to. */
function reach(chunks: readonly LogChunk[]): { lines: number; oldest: number | undefined } {
  let lines = 0;
  let oldest: number | undefined;
  for (const chunk of chunks) {
    for (const line of chunk.lines) {
      lines++;
      if (line.time) {
        const at = timestampMs(line.time);
        if (oldest === undefined || at < oldest) oldest = at;
      }
    }
  }
  return { lines, oldest };
}

/**
 * The lines around a match: a stack frame on its own says little. It reads them
 * from the source they came from, whichever search found them, and leaves the
 * search where it was.
 */
export function LogContext({
  id,
  server,
  entry,
  sources,
  onClose,
}: {
  id: string;
  server: string;
  entry: Entry;
  sources: ReadonlyMap<string, LogSource>;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const store = useMemo(() => new LogStore(), []);
  const pane = useRef<LogStreamHandle>(null);
  const [error, setError] = useState<ConnectError | undefined>();
  const [current, setCurrent] = useState<number | undefined>();
  const [done, setDone] = useState(false);
  useSyncExternalStore(store.subscribe, store.snapshot);
  const source = sources.get(entry.source);

  useEffect(() => {
    store.setDraw((e) => drawEntry(e, sources, i18n));
  }, [store, sources, i18n]);

  useEffect(() => {
    const abort = new AbortController();
    const client = createClient(LogService, transport);
    const read = (since: Timestamp, until: Timestamp, pageSize: number) =>
      client.readLogs({ id, server, sourceIds: [entry.source], since, until, pageSize }, { signal: abort.signal });
    const at = (plus: number) => timeOf(entry.ms, entry.nanos, plus);
    void (async () => {
      try {
        // A page is the newest of what its window holds, so one window that ended
        // seconds after the line could hold only what came after, in a busy log.
        // What led to it ends at it, and so holds it, however busy the seconds after.
        const before = await read(timeOf(entry.ms - BEFORE_MS, entry.nanos), at(0), BEFORE_LINES);
        // What came after starts just after it, and narrows until a page reaches back to it.
        let span = AFTER_MS * 1_000_000;
        let after = await read(at(1), at(span), AFTER_LINES);
        for (let i = 0; i < NARROWINGS; i++) {
          const { lines, oldest } = reach(after.chunks);
          if (lines < AFTER_LINES || oldest === undefined) break;
          const held = Math.max(1, Math.round((oldest - entry.ms) * 1_000_000));
          span = Math.max(1, Math.min(span - 1, span - held));
          after = await read(at(1), at(span), AFTER_LINES);
        }
        store.addOlder([...before.chunks, ...after.chunks]);
        setDone(true);
      } catch (err: unknown) {
        if (!abort.signal.aborted) setError(ConnectError.from(err));
      }
    })();
    return () => abort.abort();
  }, [store, id, server, entry, transport]);

  // Once it has them, it opens on the line it was asked about.
  useLayoutEffect(() => {
    if (!done) return;
    for (let p = store.start; p < store.start + store.length; p++) {
      const e = store.entryAt(p);
      if (e && e.ms === entry.ms && e.nanos === entry.nanos && e.text === entry.text) {
        setCurrent(e.id);
        pane.current?.show(p);
        return;
      }
    }
  }, [done, store, entry]);

  const lines = useMemo(
    () => ({
      get length() {
        return store.length;
      },
      get start() {
        return store.start;
      },
      get version() {
        return store.version;
      },
      get epoch() {
        return store.epoch;
      },
      at: (p: number) => store.at(p),
      positionOf: (id: string | number) => store.positionOf(id),
    }),
    [store],
  );

  return (
    <Sheet
      open
      onClose={onClose}
      width={720}
      className="logs-context"
      title={t`Lines around this one`}
      subtitle={source ? replicaLabel(source) : undefined}
    >
      {error ? (
        <Callout tone="critical" title={t`Can't read the lines around it`}>
          {sentence(error)}
        </Callout>
      ) : (
        <LogStream
          ref={pane}
          className="logs-context-stream"
          lines={lines}
          height="fill"
          showSource={false}
          follow={false}
          current={current}
          copyLine={selectionText}
          empty={
            done ? (
              <Trans>Nothing else was written around this line.</Trans>
            ) : (
              <span aria-busy="true">
                <Trans>Reading the lines around it</Trans>
              </span>
            )
          }
        />
      )}
    </Sheet>
  );
}
