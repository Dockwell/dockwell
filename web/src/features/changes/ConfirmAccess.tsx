// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type ChangeConfirmation, ChangeService, Connection } from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";

/**
 * A change to who reaches a server that waits for its confirmation, as ADR 0052
 * has it: the test to run first, a new SSH session from where you are, with your
 * key, how long is left before the server puts back its firewall on its own,
 * and the button that keeps the change. Only what you test is proved, so it says
 * what to test, and keeping it is yours to say.
 */
/** How long before its deadline a confirmation stops being offered. */
const KEEP_MARGIN_MS = 2000;

export function ConfirmAccess({ changeId, wait }: { changeId: string; wait: ChangeConfirmation }) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const now = useControlPlaneNow(1000);
  const servers = useFleetView().servers;
  const server = servers?.find((s) => s.id === wait.serverId);
  const name = server?.name ?? wait.serverId;
  // A server whose agent has gone can't hear a confirmation; its own timer puts
  // its firewall back.
  const gone = server !== undefined && server.connection !== Connection.CONNECTED;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const deadline = wait.deadline ? timestampDate(wait.deadline).getTime() : now;
  // Whole seconds left, never more than there are, and Keep goes a little early,
  // so a click doesn't land after the server has put its firewall back.
  const left = Math.max(0, Math.floor((deadline - now) / 1000));
  const open = deadline - now > KEEP_MARGIN_MS && !gone;
  const ports = wait.sshPorts;
  const port = ports[0] ?? 0;
  const portList = ports.join(` ${t`or`} `);
  const keep = async () => {
    setBusy(true);
    setError(undefined);
    try {
      // The server and the deadline it answers, so a second click, or one that
      // comes once another server waits, keeps nothing it wasn't for.
      await client.confirmChange({ id: changeId, serverId: wait.serverId, deadline: wait.deadline });
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Callout
      tone="warning"
      title={t`Check you can still reach ${name}`}
      className="plan-confirm"
      actions={
        <Button variant="primary" onClick={keep} disabled={busy || !open}>
          <Trans>Keep the change</Trans>
        </Button>
      }
    >
      <p>
        {ports.length === 0 ? (
          <Trans>Open a new SSH session from where you are now, with your key. If it works, keep the change.</Trans>
        ) : ports.length === 1 ? (
          <Trans>
            Open a new SSH session to port {port} from where you are now, with your key. If it works, keep the change.
          </Trans>
        ) : (
          <Trans>
            Open a new SSH session to port {portList} from where you are now, with your key. If it works, keep the
            change.
          </Trans>
        )}
      </p>
      <p className="plan-confirm-left" aria-live="polite">
        {gone ? (
          <Trans>
            {name}'s agent isn't connected, so it can't hear a confirmation. It puts back its firewall as it was on its
            own when its time runs out.
          </Trans>
        ) : open ? (
          <Trans>
            {name} puts back its firewall as it was in {left} s unless you keep it.
          </Trans>
        ) : (
          <Trans>{name} is putting back its firewall as it was.</Trans>
        )}
      </p>
      {error ? (
        <p className="plan-confirm-error" role="alert">
          {reasonOf(error) === "not_yours"
            ? t`Only the person who applied this change can keep it, from where they applied it.`
            : reasonOf(error) === "not_waiting"
              ? t`That wait is over: the server kept the change, or put back its firewall. The change goes on as this sheet shows.`
              : sentence(error)}
        </p>
      ) : null}
    </Callout>
  );
}
