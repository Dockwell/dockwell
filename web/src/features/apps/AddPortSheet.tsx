// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, Protocol, type ServiceInfo, ServiceService } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, SelectField, Sheet, TextField, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { useApp } from "./AppContext";
import { isMoving, nameOf, portRefusal, tidyMessage } from "./app";
import { validPort } from "./form";

/**
 * Publishes one more port: a port on the server and one in the app, then the
 * plan. The server merges it into what the service has, so nothing here reads
 * a secret back. Dockwell refuses at the field a port the service publishes
 * already, before there is a plan to review.
 */
export function AddPortSheet({ service, onClose }: { service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ServiceService, transport), [transport]);
  const name = nameOf(service);
  const [host, setHost] = useState("");
  const [inApp, setInApp] = useState("");
  const [udp, setUdp] = useState(false);
  const [busy, setBusy] = useState(false);
  const [hostError, setHostError] = useState<string>();
  const [appError, setAppError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  // The plan under review takes this sheet over, and Cancel brings the form back as it was.
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLInputElement>(null);
  const { state } = useApp();
  // One change at a time: this one's plan couldn't apply until that one ends.
  const moving = isMoving(state);

  // Back from a plan that failed, what it said is at the field, ready to change.
  const reveal = useRef(false);
  useEffect(() => {
    if (plan || !reveal.current) return;
    reveal.current = false;
    field.current?.focus();
    field.current?.select();
  }, [plan]);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy || moving) return;
    const hostOk = validPort(host);
    const appOk = validPort(inApp);
    setHostError(hostOk ? undefined : t`Use a port from 1 to 65535.`);
    setAppError(appOk ? undefined : t`Use a port from 1 to 65535.`);
    setFailure(undefined);
    if (!hostOk || !appOk) return;
    setBusy(true);
    let change: Change | undefined;
    try {
      const hostPort = Number(host);
      change = (
        await client.addServicePort({
          id: service.id,
          hostPort,
          containerPort: Number(inApp),
          protocol: udp ? Protocol.UDP : Protocol.TCP,
          summary: t`Publish port ${hostPort} on ${name}`,
        })
      ).change;
    } catch (err) {
      const taken = reasonOf(err) === "agent_failed" ? portRefusal(sentence(err)) : undefined;
      if (taken) setHostError(taken);
      else if (reasonOf(err) === "invalid") setHostError(sentence(err));
      else setFailure(sentence(err));
      setBusy(false);
      return;
    }
    setBusy(false);
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Edit and try again`,
      onDone: onClose,
      fix: (reason) =>
        // A port is all that moved, and its numbers are all this form has. A check that fails says
        // what it answered, and what to do about that, which is the server's.
        reason === "exits" ? t`Check the port in the app, then try again.` : undefined,
      onClose: (after) => {
        // Cancelling comes back here, with what you typed, as failing does, saying why
        // at the field. Anything else is under way or over.
        if (after?.state === ChangeState.FAILED) {
          setHostError(tidyMessage(after.error));
          reveal.current = true;
          setPlan(undefined);
        } else if (!after || after.state === ChangeState.PLANNED) setPlan(undefined);
        else onClose();
      },
    });
  };

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={t`Add a port to ${name}`}
      subtitle={t`People outside the server reach the app through this port`}
      onClose={onClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing changes until you apply the plan</Trans>
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={busy}
            disabled={moving}
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void submit()}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        className="app-form"
        onSubmit={(event) => void submit(event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void submit(event);
        }}
      >
        {/* Lets Enter in a field do what the button in the sheet's footer does. */}
        <button type="submit" hidden />
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        {moving ? (
          <Callout tone="info">
            <Trans>{name} is still deploying. You can review this plan once that ends.</Trans>
          </Callout>
        ) : null}
        <TextField
          ref={field}
          label={t`On the server`}
          mono
          inputMode="numeric"
          value={host}
          error={hostError}
          hint={t`What people connect to, such as 8080.`}
          onChange={(event) => {
            setHost(event.target.value);
            setHostError(undefined);
          }}
        />
        <TextField
          label={t`In the app`}
          mono
          inputMode="numeric"
          value={inApp}
          error={appError}
          hint={t`What the app listens on, such as 80.`}
          onChange={(event) => {
            setInApp(event.target.value);
            setAppError(undefined);
          }}
        />
        <SelectField
          label={t`Protocol`}
          value={udp ? "udp" : "tcp"}
          options={[
            { value: "tcp", label: "TCP" },
            { value: "udp", label: "UDP" },
          ]}
          onChange={(event) => setUdp(event.target.value === "udp")}
        />
      </form>
    </Sheet>
  );
}
