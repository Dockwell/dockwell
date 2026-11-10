// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type AccessStays, type ChangeConfirmation, ChangeService, Connection } from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useSignedInUser } from "../auth/useAuthState";
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

export function ConfirmAccess({
  changeId,
  wait,
  stays,
  appliedBy,
}: {
  changeId: string;
  wait: ChangeConfirmation;
  /** Who SSH stays open from once this is kept, as the plan says, when it narrows SSH. */
  stays?: AccessStays;
  /** The ID of whoever applied the change: only they can keep it. */
  appliedBy?: string;
}) {
  const { t } = useLingui();
  const me = useSignedInUser();
  // Someone else watching: the server would refuse them, so they aren't offered it.
  const theirs = appliedBy !== undefined && appliedBy !== "" && me !== undefined && me.id !== appliedBy;
  // Keeping a change to who reaches a server is an admin's, on the install (ADR 0059).
  const mayKeep = useMay()(ChangeService.method.confirmChange);
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
  const from = stays && stays.sshPorts.length > 0 ? [...stays.pinned, ...stays.allowed].join(", ") : "";
  const panelPorts = (stays?.panelPorts.length ?? 0) > 0;
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
        theirs ? undefined : (
          <Button variant="primary" onClick={keep} disabled={busy || !open} {...mayKeep.off}>
            <Trans>Keep the change</Trans>
          </Button>
        )
      }
    >
      <p>
        {ports.length === 0 ? (
          <Trans>Open a new SSH session from here with your key, and keep the change if it works.</Trans>
        ) : ports.length === 1 ? (
          <Trans>Open a new SSH session to port {port} from here with your key, and keep the change if it works.</Trans>
        ) : (
          <Trans>
            Open a new SSH session to port {portList} from here with your key, and keep the change if it works.
          </Trans>
        )}
      </p>
      {panelPorts ? (
        <p>
          <Trans>Then open the panel at its own address in a new tab.</Trans>
        </p>
      ) : null}
      {from ? (
        <p>
          <Trans>Once kept, SSH stays open only from {from}.</Trans>
        </p>
      ) : null}
      {theirs ? (
        <p>
          <Trans>Only the person who applied this change can keep it, from where they applied it.</Trans>
        </p>
      ) : null}
      <p className="plan-confirm-left" aria-live="polite">
        {gone ? (
          <Trans>{name}'s agent isn't connected, so it puts back its firewall when its time runs out.</Trans>
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
              ? t`That wait is over: this sheet shows whether the server kept the change.`
              : sentence(error)}
        </p>
      ) : null}
    </Callout>
  );
}
