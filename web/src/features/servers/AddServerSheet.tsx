// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  Connection,
  JoinReach,
  type JoinTicket,
  JoinTicketState,
  ServerService,
  StepUpAction,
  UpdateService,
} from "@dockwell/api";
import { Button, Callout, Sheet, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { useNow } from "../../lib/time";
import { PasskeyError } from "../../lib/webauthn";
import { useMay } from "../auth/may";
import { StepUpDeclined, withStepUpIfAsked } from "../auth/stepUp";
import { shortVersion } from "../shell/version";
import { CopyCommand } from "../system/CopyCommand";
import { installCommand } from "../updates/updates";
import { SetPublicAddressSheet } from "./SetPublicAddressSheet";
import { useFleet } from "./useFleet";
import "./ServersPage.css";

/**
 * What a new server runs to join, with the ticket in its place: the install script of
 * the control plane's own release, so its agent is that version, from where the
 * control plane reads releases, as the Updates tab says to update an agent. Undefined
 * where either isn't known, as for a build that isn't a release.
 */
export function joinCommand(releases: string, controlPlane: string, ticket = "<ticket>"): string | undefined {
  // A build on the way to a release, as the rail reads it, has no install script of its own to fetch.
  if (shortVersion(controlPlane).endsWith("dev")) return undefined;
  const install = installCommand(releases, controlPlane);
  return install ? `${install} --join ${ticket}` : undefined;
}

/**
 * Adds a server as Add-Server has it: the panel makes the ticket, with a passkey, and
 * shows the one command to run on the new server with it in place; then it waits, and
 * says when the server has joined, with the way to its page. What the ticket's address
 * means for a server elsewhere is a state with its fix, the server's public address.
 */
export function AddServerSheet({
  control,
  onClose,
}: {
  /** The control server, whose release the new one's agent takes. */
  control: { id: string; name: string } | undefined;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(ServerService, transport), [transport]);
  const fleet = useFleet();
  const servers = fleet.status === "connecting" ? undefined : fleet.servers;
  const updates = useQuery(
    UpdateService.method.getServerUpdates,
    { id: control?.id ?? "" },
    { enabled: control !== undefined, retry: false },
  );
  const [made, setMade] = useState<{ ticket: JoinTicket; value: string }>();
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const [addressing, setAddressing] = useState(false);
  const may = useMay();
  const mayMake = may(ServerService.method.createJoinTicket);
  const maySetAddress = may(ServerService.method.setServerPublicAddress);
  const now = useNow(15_000);
  const read = useQuery(
    ServerService.method.getJoinTicket,
    { id: made?.ticket.id ?? "" },
    {
      enabled: made !== undefined,
      // Until a server joins with it or it runs out; then the fleet says the rest.
      refetchInterval: (query) => {
        const state = query.state.data?.ticket?.state;
        return state === undefined || state === JoinTicketState.WAITING ? 2000 : false;
      },
    },
  );

  // How long until a time, in the largest whole unit: "59 min", "23 h". A closure, as t works only here.
  const left = (ms: number) => {
    const hours = Math.floor(ms / 3_600_000);
    if (hours >= 1) return t`${hours} h`;
    const minutes = Math.max(1, Math.floor(ms / 60_000));
    return t`${minutes} min`;
  };
  const name = control?.name;
  const controlServer = servers?.find((s) => s.id === control?.id);
  const release = updates.data?.dockwell;
  const expires = made?.ticket.expireTime ? timestampDate(made.ticket.expireTime).getTime() : 0;
  const ticketState = read.data?.ticket?.state ?? made?.ticket.state;
  const state =
    ticketState === JoinTicketState.WAITING && expires > 0 && now >= expires ? JoinTicketState.EXPIRED : ticketState;
  const joinedId = read.data?.ticket?.serverId ?? "";
  const joined = servers?.find((s) => s.id === joinedId);
  const joinedName = joined?.name || read.data?.ticket?.serverName || joinedId;

  const make = async () => {
    if (busy) return;
    setBusy(true);
    setFailure(undefined);
    try {
      const res = await withStepUpIfAsked(transport, StepUpAction.ADD_SERVER, "", (proof) =>
        client.createJoinTicket({ proof }),
      );
      if (res.ticket) setMade({ ticket: res.ticket, value: res.value });
    } catch (err) {
      if (err instanceof StepUpDeclined) return;
      setFailure(
        err instanceof PasskeyError || err instanceof DOMException
          ? t`Your passkey didn't confirm it, so there's no command yet. Try again.`
          : sentence(err),
      );
    } finally {
      setBusy(false);
    }
  };

  const first = made?.ticket.addresses[0];
  const host = first ? hostOf(first.url) : "";
  // The control server by name, or by where the ticket leads where its name isn't known.
  const controlName = name ?? host;
  const install = made ? joinCommand(release?.releases ?? "", release?.controlPlane ?? "", made.value) : undefined;
  const version = release?.controlPlane ? shortVersion(release.controlPlane) : "";

  let body: ReactNode;
  if (!made) {
    body = (
      <section className="servers-add-step">
        <h3>
          <Trans>Run one command on the new server</Trans>
        </h3>
        <p>
          <Trans>Any Linux server you have root on: another provider, a home lab or a VM.</Trans>
        </p>
        {failure ? (
          <Callout tone="critical" title={t`No command yet`}>
            {failure}
          </Callout>
        ) : null}
        <div className="servers-add-make">
          <Button variant="primary" icon="key" loading={busy} onClick={() => void make()} {...mayMake.off}>
            {busy ? t`Making the command` : t`Make the command`}
          </Button>
          <span className="servers-add-meta">
            <Trans>Confirm with your passkey. The command works once, for an hour.</Trans>
          </span>
        </div>
      </section>
    );
  } else if (state === JoinTicketState.JOINED) {
    const connected = joined?.connection === Connection.CONNECTED;
    body = (
      <section className="servers-add-step" aria-live="polite">
        <div className="servers-add-done">
          <StatusBadge
            status={connected ? "healthy" : "deploying"}
            label={connected ? t`${joinedName} joined` : t`${joinedName} joined, starting its agent`}
            labelIsState
          />
          {joinedId ? (
            <RouterButtonLink to="/servers/$serverId" params={{ serverId: joinedId }} size="sm" icon="arrow-right">
              <Trans>Open {joinedName}</Trans>
            </RouterButtonLink>
          ) : null}
        </div>
        <p>
          <Trans>Your apps stay where they are. New ones ask where to run.</Trans>
        </p>
      </section>
    );
  } else {
    const expired = state === JoinTicketState.EXPIRED;
    body = (
      <section className="servers-add-step">
        <h3>
          {install ? <Trans>Run this on the new server</Trans> : <Trans>Run these on the new server, as root</Trans>}
        </h3>
        {install ? (
          <CopyCommand command={install} className="servers-add-command" />
        ) : (
          <>
            <p>
              {version ? (
                <Trans>
                  Install the agent of <code>{version}</code> as <code>dockwell</code> first, as this build has no
                  installer.
                </Trans>
              ) : (
                <Trans>
                  Install an agent of the control server's version as <code>dockwell</code> first.
                </Trans>
              )}
            </p>
            <CopyCommand command={`dockwell join ${made.value}`} className="servers-add-command" />
            <CopyCommand command="dockwell run" />
          </>
        )}
        <div className="servers-add-wait" aria-live="polite">
          {expired ? (
            <>
              <StatusBadge status="stopped" label={t`This command expired`} labelIsState />
              <Button size="sm" icon="key" loading={busy} onClick={() => void make()} {...mayMake.off}>
                <Trans>Make a new command</Trans>
              </Button>
            </>
          ) : (
            <>
              <StatusBadge status="deploying" label={t`Waiting for the server to join`} labelIsState />
              <span className="servers-add-meta">{t`Works once · expires in ${left(expires - now)}`}</span>
            </>
          )}
        </div>
        {first?.reach === JoinReach.SAME_NETWORK || first?.reach === JoinReach.WAY_OUT ? (
          <Callout
            tone="warning"
            title={
              first.reach === JoinReach.SAME_NETWORK
                ? t`Only servers on ${controlName}'s network can join`
                : t`${host} may not lead back to ${controlName}`
            }
            actions={
              controlServer ? (
                <Button size="sm" onClick={() => setAddressing(true)} {...maySetAddress.off}>
                  <Trans>Set public address</Trans>
                </Button>
              ) : null
            }
          >
            {first.reach === JoinReach.SAME_NETWORK
              ? t`It joins through ${host}, a private address.`
              : t`It's the address ${controlName} goes out from.`}
          </Callout>
        ) : host ? (
          <p className="servers-add-meta">{t`It reaches ${controlName} at ${host}.`}</p>
        ) : null}
      </section>
    );
  }

  return (
    <Sheet
      open
      title={t`Add a server`}
      subtitle={name ? t`It joins in seconds. Nothing on ${name} changes.` : t`It joins in seconds.`}
      onClose={onClose}
      className="servers-add"
      footer={
        <Button variant={state === JoinTicketState.JOINED ? "primary" : "secondary"} onClick={onClose}>
          <Trans>Done</Trans>
        </Button>
      }
    >
      {body}
      {addressing && controlServer ? (
        <SetPublicAddressSheet
          server={controlServer}
          current={controlServer.network?.ipv4}
          onClose={() => {
            setAddressing(false);
            // The ticket holds the address it was made with: the next one leads with the new one.
            setMade(undefined);
          }}
        />
      ) : null}
    </Sheet>
  );
}

/** The host and port of a URL, as a person reads where something is. */
function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}
