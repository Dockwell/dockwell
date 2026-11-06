// SPDX-License-Identifier: AGPL-3.0-only

import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { LogService, type LogSource } from "@dockwell/api";
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
const LINES = 500;

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
    void createClient(LogService, transport)
      .readLogs(
        {
          id,
          server,
          sourceIds: [entry.source],
          since: timestampFromMs(entry.ms - BEFORE_MS),
          until: timestampFromMs(entry.ms + AFTER_MS),
          pageSize: LINES,
        },
        { signal: abort.signal },
      )
      .then((res) => {
        store.addOlder(res.chunks);
        setDone(true);
      })
      .catch((err: unknown) => {
        if (!abort.signal.aborted) setError(ConnectError.from(err));
      });
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
