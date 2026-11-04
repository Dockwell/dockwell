// SPDX-License-Identifier: AGPL-3.0-only

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

/**
 * Runs connect again each time it ends or fails, until it returns "done"
 * or signal aborts, as a watch reconnects. Between tries it waits, with
 * jitter so many tabs don't come back at once, from backoff.minMs up to
 * backoff.maxMs; connect calls connected once it hears from the server,
 * which starts the waits over. failed hears each failure, and may return
 * "done" for one that trying again won't fix.
 */
export async function keepTrying(
  signal: AbortSignal,
  backoff: Backoff,
  connect: (connected: () => void) => Promise<"done" | undefined>,
  failed?: (err: unknown) => "done" | undefined,
): Promise<void> {
  let wait = backoff.minMs;
  while (!signal.aborted) {
    try {
      if ((await connect(() => (wait = backoff.minMs))) === "done") return;
    } catch (err) {
      if (signal.aborted || failed?.(err) === "done") return;
    }
    await delay(wait + Math.random() * wait, signal);
    wait = Math.min(wait * 2, backoff.maxMs);
  }
}
