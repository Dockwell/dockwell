// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Process, ProcessService, type Server, StepUpAction } from "@dockwell/api";
import { Button, Callout, Sheet } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { withStepUpIfAsked } from "../auth/stepUp";
import type { Owner } from "./processes";

/**
 * Stops a process, once its facts and what stopping it does are in front of
 * whoever asked: SIGTERM, then SIGKILL after 10 s. It's as much as root, so it
 * asks for a passkey, and it goes on Activity.
 */
export function EndProcessSheet({
  server,
  process,
  owner,
  onClose,
}: {
  server: Server;
  process: Process;
  owner: Owner;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const [done, setDone] = useState<string>();
  const name = process.name;
  const pid = process.pid;

  const end = async () => {
    if (busy) return;
    setBusy(true);
    setFailure(undefined);
    try {
      const client = createClient(ProcessService, transport);
      // The proof names when the process started too, so it ends no other that gets its PID.
      const target = `${server.id}:${pid}:${process.startTicks}`;
      const out = await withStepUpIfAsked(transport, StepUpAction.END_PROCESS, target, (proof) =>
        client.endProcess({ serverId: server.id, pid, startTicks: process.startTicks, proof }),
      );
      setDone(out.killed ? t`${name} didn't stop within 10 s, so it was killed.` : t`${name} stopped.`);
    } catch (err) {
      setFailure(
        reasonOf(err) === "needs_step_up" || err instanceof DOMException
          ? t`Your passkey didn't confirm it, so ${name} still runs. Try again, and confirm when your browser asks.`
          : sentence(err),
      );
    } finally {
      setBusy(false);
    }
  };

  let what: ReactNode;
  switch (owner.kind) {
    case "app":
      what = (
        <AgainInContainer
          process={process}
          where={
            <Trans>
              app <strong>{owner.name}</strong>
            </Trans>
          }
        >
          <Trans>To stop the app, stop it on its own page.</Trans>
        </AgainInContainer>
      );
      break;
    case "container":
      what = <AgainInContainer process={process} where={<Trans>container {owner.name}</Trans>} />;
      break;
    case "unit":
      what = <Trans>It runs under {owner.name}.service, which may start it again, as the unit says.</Trans>;
      break;
    default:
      what = null;
  }

  return (
    <Sheet
      open
      title={done ? t`Stopped ${name}` : t`Stop ${name}?`}
      subtitle={t`PID ${pid} on ${server.name}`}
      onClose={onClose}
      footer={
        done ? (
          <Button variant="primary" onClick={onClose}>
            <Trans>Done</Trans>
          </Button>
        ) : (
          <>
            <span className="dw-sheet-note">
              <Trans>It asks for your passkey, and goes on Activity</Trans>
            </span>
            <Button variant="ghost" onClick={onClose}>
              <Trans>Cancel</Trans>
            </Button>
            <Button variant="danger" icon="stop" loading={busy} onClick={() => void end()}>
              <Trans>Stop process</Trans>
            </Button>
          </>
        )
      }
    >
      <div className="procs-end">
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        {done ? (
          <p>{done}</p>
        ) : (
          <>
            <p>
              <Trans>
                It gets SIGTERM, to end as it would on shutdown, and SIGKILL if it's still running 10 s later. What it
                hasn't saved is lost, and this can't be undone.
              </Trans>
            </p>
            {process.stops === "containers" ? (
              <Callout tone="warning">
                <Trans>Every container on {server.name} stops with it, as Docker needs it to run them.</Trans>
              </Callout>
            ) : process.stops === "container" ? (
              <Callout tone="warning">
                <Trans>Its container stops with it, as this is what runs the container.</Trans>
              </Callout>
            ) : null}
            {what ? <p>{what}</p> : null}
            <dl className="facts">
              <dt>{t`Command`}</dt>
              <dd>
                <code>{process.command || process.name}</code>
              </dd>
              <dt>{t`User`}</dt>
              <dd>
                <code>{process.user}</code>
              </dd>
              <dt>{t`Memory`}</dt>
              <dd>{formatBytes(process.memoryBytes, i18n.locale)}</dd>
            </dl>
          </>
        )}
      </div>
    </Sheet>
  );
}

/**
 * Whether Docker starts a process in a container again: only its main process, and
 * only as the container's restart policy says, which for "on-failure" leaves out
 * one that ends cleanly on SIGTERM.
 */
function AgainInContainer({ process, where, children }: { process: Process; where: ReactNode; children?: ReactNode }) {
  if (!process.containerMain) {
    return (
      <Trans>
        It runs in {where}, but isn't its main process, so nothing starts it again, and the container goes on running.
      </Trans>
    );
  }
  switch (process.container?.restartPolicy) {
    case "always":
    case "unless-stopped":
      return (
        <>
          <Trans>It's the main process of {where}, which Docker starts again, as its restart policy says.</Trans>{" "}
          {children}
        </>
      );
    case "on-failure":
      return (
        <Trans>
          It's the main process of {where}, which stops with it. Docker starts it again only if it ends with an error,
          and a process that takes SIGTERM can end without one.
        </Trans>
      );
    case "no":
      return (
        <Trans>
          It's the main process of {where}, which stops with it, and Docker doesn't start it again, as its restart
          policy is no.
        </Trans>
      );
    default:
      return (
        <Trans>
          It's the main process of {where}, which stops with it, and may start again, as its restart policy says.
        </Trans>
      );
  }
}
