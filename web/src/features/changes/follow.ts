// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeService } from "@dockwell/api";
import { useCallback, useEffect, useRef } from "react";

import { isSessionEnded } from "../../lib/errors";
import { keepTrying } from "../../lib/retry";
import { isFinal } from "./plan";

const BACKOFF = { minMs: 500, maxMs: 8_000 };

/**
 * Follows a change with ChangeService.WatchChange until it is applied,
 * failed or discarded, calling onChange with the change as it stands after
 * each event: the change itself, or one of its steps moving on. A dropped
 * watch starts again, and its first message brings the change up to date.
 * It returns once the change is over or signal is aborted.
 */
export async function followChange(
  transport: Transport,
  id: string,
  signal: AbortSignal,
  onChange: (change: Change) => void,
): Promise<void> {
  const client = createClient(ChangeService, transport);
  let current: Change | undefined;
  await keepTrying(
    signal,
    BACKOFF,
    async (connected, attempt) => {
      for await (const msg of client.watchChange({ id }, { signal: attempt })) {
        if (msg.event.case === "change") {
          current = msg.event.value;
        } else if (msg.event.case === "step" && current) {
          const { index, step } = msg.event.value;
          if (step && index >= 0 && index < current.steps.length) {
            const steps = current.steps.slice();
            steps[index] = step;
            current = { ...current, steps };
          }
        }
        if (current) onChange(current);
        connected();
      }
      return !current || isFinal(current) ? "done" : undefined;
    },
    (err) => {
      const code = ConnectError.from(err).code;
      // A change that isn't there, or a session that ended, won't come back by trying again.
      return code === Code.NotFound || code === Code.PermissionDenied || isSessionEnded(err) ? "done" : undefined;
    },
  );
}

/**
 * What an opening's onClose is for a screen that reads what a change changes
 * with no watch to say it moved, as what a server's agent reads: it reads at
 * once, as the sheet closes, and once more as the change ends, if the sheet
 * closed before that. A sheet closed as its change applies, as its × and Close
 * let you, would otherwise leave the read it took as it closed, from before
 * the change reached the server, until the screen's next one.
 */
export function useReadAgainOnceOver(read: () => void): (change: Change | undefined) => void {
  const transport = useTransport();
  const reading = useRef(read);
  reading.current = read;
  const following = useRef(new Set<AbortController>());
  useEffect(() => {
    const all = following.current;
    return () => {
      for (const abort of all) abort.abort();
      all.clear();
    };
  }, []);
  return useCallback(
    (change: Change | undefined) => {
      reading.current();
      // One that waits for an admin may wait for days: the screen's own reads say how it went.
      if (!change || isFinal(change) || change.waits !== "") return;
      const abort = new AbortController();
      following.current.add(abort);
      void followChange(transport, change.id, abort.signal, (now) => {
        if (abort.signal.aborted || !isFinal(now)) return;
        abort.abort();
        following.current.delete(abort);
        reading.current();
      });
    },
    [transport],
  );
}
