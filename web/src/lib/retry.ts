// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";

/** Waits ms, or less if signal aborts first. */
export function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
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

/** How long to wait between tries: from minMs, doubling each time, up to maxMs. */
export interface Backoff {
  minMs: number;
  maxMs: number;
}

/** What a try that the browser going offline cut short fails with. */
export const OFFLINE = new ConnectError("this browser is offline", Code.Unavailable);

/** A signal that aborts with signal, and as the browser goes offline, as a stream that stays open says nothing then. */
function tryingSignal(signal: AbortSignal): { signal: AbortSignal; done: () => void } {
  const each = new AbortController();
  const stop = () => each.abort(signal.reason);
  const offline = () => each.abort(OFFLINE);
  signal.addEventListener("abort", stop, { once: true });
  window.addEventListener("offline", offline);
  return {
    signal: each.signal,
    done: () => {
      signal.removeEventListener("abort", stop);
      window.removeEventListener("offline", offline);
    },
  };
}

/**
 * Runs connect again each time it ends or fails, until it returns "done"
 * or signal aborts, as a watch reconnects. Between tries it waits, with
 * jitter so many tabs don't come back at once, from backoff.minMs up to
 * backoff.maxMs; connect calls connected once it hears from the server,
 * which starts the waits over. failed hears each failure, and may return
 * "done" for one that trying again won't fix. Each try has a signal of its
 * own, for its calls, which also aborts as the browser goes offline, with
 * OFFLINE: what it watches can't be live then, whatever its stream says.
 */
export async function keepTrying(
  signal: AbortSignal,
  backoff: Backoff,
  connect: (connected: () => void, attempt: AbortSignal) => Promise<"done" | undefined>,
  failed?: (err: unknown) => "done" | undefined,
): Promise<void> {
  let wait = backoff.minMs;
  while (!signal.aborted) {
    const attempt = tryingSignal(signal);
    try {
      if ((await connect(() => (wait = backoff.minMs), attempt.signal)) === "done") return;
    } catch (err) {
      if (signal.aborted || failed?.(attempt.signal.reason === OFFLINE ? OFFLINE : err) === "done") return;
    } finally {
      attempt.done();
    }
    await delay(wait + Math.random() * wait, signal);
    wait = Math.min(wait * 2, backoff.maxMs);
  }
}
