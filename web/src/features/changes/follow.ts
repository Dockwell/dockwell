// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { type Change, ChangeService } from "@dockwell/api";

import { isSessionEnded } from "../../lib/errors";
import { isFinal } from "./plan";

const MIN_BACKOFF_MS = 500;
const MAX_BACKOFF_MS = 8_000;

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
  let backoff = MIN_BACKOFF_MS;
  let current: Change | undefined;
  while (!signal.aborted) {
    try {
      for await (const msg of client.watchChange({ id }, { signal })) {
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
        backoff = MIN_BACKOFF_MS;
      }
      if (!current || isFinal(current)) return;
    } catch (err) {
      if (signal.aborted) return;
      const code = ConnectError.from(err).code;
      // A change that isn't there, or a session that ended, won't come back by trying again.
      if (code === Code.NotFound || code === Code.PermissionDenied || isSessionEnded(err)) return;
    }
    await delay(backoff + Math.random() * backoff, signal);
    backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
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
