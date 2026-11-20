// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import { AlertService } from "@dockwell/api";
import { Button, type ButtonProps, cx } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { tookMs } from "./checks";

/**
 * Asks a check's address again now, as its own run would, and says what it answered
 * beside the button (#832): so whoever just fixed a site learns at once, not at the
 * check's next run. The run is kept with the check's others, and what reads the
 * checks and alerts is read again.
 *
 * Compact, in a table row, the row's own state and last answers say how the run went,
 * so its words are for screen readers only; a run that couldn't start says why under
 * the button, wrapped to its width, so the column never grows past the table.
 */
export function CheckNow({
  name,
  size = "sm",
  className,
  compact = false,
}: {
  name: string;
  size?: ButtonProps["size"];
  className?: string;
  compact?: boolean;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = useQueryClient();
  const may = useMay()(AlertService.method.runAlertCheck, { install: true });
  const [busy, setBusy] = useState(false);
  const [said, setSaid] = useState<{ words: string; ok: boolean; ran: boolean }>();
  if (!may.allowed) return null;
  const run = async () => {
    setBusy(true);
    setSaid(undefined);
    try {
      const res = await createClient(AlertService, transport).runAlertCheck({ name });
      const last = res.check?.runs[0];
      const ms = last ? tookMs(last) : undefined;
      if (last?.ok) {
        setSaid({
          ok: true,
          ran: true,
          words: ms === undefined ? t`Answered ${last.status}` : t`Answered ${last.status} in ${ms} ms`,
        });
      } else {
        setSaid({ ok: false, ran: true, words: t`Didn't answer as it should: ${last?.said ?? ""}` });
      }
      await client.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: AlertService, cardinality: undefined }),
      });
    } catch (err) {
      setSaid({ ok: false, ran: false, words: sentence(err) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <span className={cx("alerts-check-now", compact && "is-compact", className)}>
      <Button
        variant="secondary"
        size={size}
        icon="refresh"
        loading={busy}
        onClick={(event) => {
          // On a row that opens the check, this asks it and opens nothing.
          event.stopPropagation();
          void run();
        }}
      >
        {t`Check now`}
      </Button>
      <span
        role="status"
        className={cx(
          "alerts-check-now-said",
          said && !said.ok && "is-failed",
          compact && (!said || said.ran) && "dw-sr",
        )}
      >
        {said?.words ?? ""}
      </span>
    </span>
  );
}
