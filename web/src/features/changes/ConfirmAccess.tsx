// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type ChangeConfirmation, ChangeService } from "@dockwell/api";
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
export function ConfirmAccess({ changeId, wait }: { changeId: string; wait: ChangeConfirmation }) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const now = useControlPlaneNow(1000);
  const servers = useFleetView().servers;
  const name = servers?.find((s) => s.id === wait.serverId)?.name ?? wait.serverId;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const deadline = wait.deadline ? timestampDate(wait.deadline).getTime() : now;
  const left = Math.max(0, Math.ceil((deadline - now) / 1000));
  const port = wait.sshPorts.length > 0 ? wait.sshPorts.join(", ") : "22";
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
        <Button variant="primary" onClick={keep} disabled={busy || left === 0}>
          <Trans>Keep the change</Trans>
        </Button>
      }
    >
      <p>
        <Trans>
          Open a new SSH session to port {port} from where you are now, with your key. If it works, keep the change.
        </Trans>
      </p>
      <p className="plan-confirm-left" aria-live="polite">
        {left > 0 ? (
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
            : sentence(error)}
        </p>
      ) : null}
    </Callout>
  );
}
