// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { type Change, ChangeService } from "@dockwell/api";

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
    async (connected) => {
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
