// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, Protocol, type ServiceInfo, ServiceService } from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  cx,
  Icon,
  PHONE_QUERY,
  PortField,
  SegmentedControl,
  Sheet,
  TextField,
  useMediaQuery,
  validPortNumber,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useMemo, useRef, useState } from "react";

import { methodCommand } from "../../lib/cli";
import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { type PortHolder, useServerPorts } from "../servers/ports";
import { useFleetView } from "../servers/useFleet";
import { CopyAsCommand } from "../system/CopyCommand";
import { useApp } from "./AppContext";
import { failedAtField, isMoving, nameOf, portRefusal } from "./app";

/** Where a published port answers: on every address of the server, or only on the server itself. */
type Reach = "anyone" | "server";

/**
 * Publishes one more port. A person says what the app listens on; Dockwell
 * picks a free port on the server for it, says as it's typed whether another is
 * free and what holds it if not, and shows the address it will answer at. The
 * protocol and who may reach it start at TCP and anyone, under Advanced. The
 * server merges the port into what the service has, so nothing here reads a
 * secret back, and the plan's own check still has the last word: it sees what
 * runs on the server outside Docker, which this form can't.
 */
export function AddPortSheet({ service, onClose }: { service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ServiceService, transport), [transport]);
  const name = nameOf(service);
  const { servers } = useFleetView();
  const serverIds = useMemo(() => service.deployments.map((d) => d.serverId), [service.deployments]);
  const ports = useServerPorts(serverIds, service.id);
  const [inApp, setInApp] = useState("");
  // The server's port follows what's free until someone types one.
  const [typedHost, setTypedHost] = useState<string>();
  // In the field, what it shows holds still: a free port landing between the click and
  // the first key would run into what's typed, so it's offered beside it instead.
  const [heldHost, setHeldHost] = useState<string>();
  const [udp, setUdp] = useState(false);
  const [reach, setReach] = useState<Reach>("anyone");
  const [advanced, setAdvanced] = useState(false);
  const [busy, setBusy] = useState(false);
  const [hostError, setHostError] = useState<string>();
  const [appError, setAppError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  // The plan under review takes this sheet over, and Cancel brings the form back as it was.
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLInputElement>(null);
  const hostField = useRef<HTMLInputElement>(null);
  const { state } = useApp();
  // One change at a time: this one's plan couldn't apply until that one ends.
  const moving = isMoving(state);

  const listensOn = validPortNumber(inApp) ? Number(inApp) : undefined;
  const offered = ports.ready ? ports.free(listensOn, udp) : undefined;
  const host = typedHost ?? heldHost ?? (offered === undefined ? "" : String(offered));
  const holderName = (holder: PortHolder) => (holder.kind === "proxy" ? t`Dockwell's proxy` : holder.name);
  const check = ports.check(host, udp, holderName);

  // Back from a plan that failed, what it said is at the field, ready to change.
  const reveal = useRef(false);
  useEffect(() => {
    if (plan || !reveal.current) return;
    reveal.current = false;
    hostField.current?.focus();
    hostField.current?.select();
  }, [plan]);

  // A port is all that moved, and its numbers are all this form has. A check that fails says
  // what it answered, and what to do about that, which is the server's.
  const fix = (reason: string) => (reason === "exits" ? t`Check the port in the app, then try again.` : undefined);

  const request = {
    id: service.id,
    hostPort: Number(host),
    containerPort: Number(inApp),
    protocol: udp ? Protocol.UDP : Protocol.TCP,
    hostIp: reach === "server" ? "127.0.0.1" : "",
  };

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy || moving) return;
    const appOk = validPortNumber(inApp);
    const hostOk = validPortNumber(host);
    setAppError(appOk ? undefined : t`Use a port from 1 to 65535.`);
    setHostError(hostOk ? undefined : t`Use a port from 1 to 65535.`);
    setFailure(undefined);
    if (!appOk) {
      field.current?.focus();
      return;
    }
    if (!hostOk) {
      hostField.current?.focus();
      return;
    }
    setBusy(true);
    let change: Change | undefined;
    try {
      const hostPort = Number(host);
      change = (await client.addServicePort({ ...request, summary: t`Publish port ${hostPort} on ${name}` })).change;
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
      fix,
      onClose: (after) => {
        // Cancelling comes back here, with what you typed, as failing does, saying why
        // at the field. Anything else is under way or over.
        if (after?.state === ChangeState.FAILED) {
          setTypedHost(host);
          setHostError(failedAtField(after, name, fix));
          reveal.current = true;
          setPlan(undefined);
        } else if (!after || after.state === ChangeState.PLANNED) setPlan(undefined);
        else onClose();
      },
    });
  };

  // Where people will reach it, as it's typed: the first server it runs on, at the port.
  const address = service.deployments
    .map((d) => servers?.find((s) => s.id === d.serverId)?.address ?? d.serverName)
    .find((a) => a !== "");
  const at =
    address && validPortNumber(host)
      ? `${address.includes(":") ? `[${address}]` : address}:${Number(host)}`
      : undefined;
  const where = reach === "server" ? `127.0.0.1:${Number(host)}` : at;

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={t`Add a port to ${name}`}
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
            {service.rolledBackChangeId !== "" ? (
              <Trans>Dockwell is putting {name} back. You can review this plan once that ends.</Trans>
            ) : (
              <Trans>{name} is still deploying. You can review this plan once that ends.</Trans>
            )}
          </Callout>
        ) : null}
        <TextField
          ref={field}
          label={t`In the app`}
          mono
          inputMode="numeric"
          placeholder="80"
          value={inApp}
          error={appError}
          hint={t`What the app listens on, such as 80.`}
          onChange={(event) => {
            setInApp(event.target.value);
            setAppError(undefined);
          }}
        />
        <PortField
          ref={hostField}
          label={t`On the server`}
          value={host}
          check={check}
          suggestion={offered}
          error={hostError}
          onFocus={() => setHeldHost(host)}
          onBlur={() => setHeldHost(undefined)}
          onChange={(next) => {
            setTypedHost(next);
            setHostError(undefined);
          }}
        />
        {where && !udp && check.state !== "taken" ? (
          <p className="app-port-at">
            <Icon name="globe" size={14} />
            <Trans>
              Reachable at <code className="app-port-at-where">{where}</code>
            </Trans>
          </p>
        ) : null}
        <Advanced
          open={advanced}
          onOpenChange={setAdvanced}
          summary={
            reach === "server"
              ? udp
                ? t`UDP · only from the server`
                : t`TCP · only from the server`
              : udp
                ? t`UDP · from anywhere`
                : t`TCP · from anywhere`
          }
        >
          <div className="app-field">
            <span className="dw-field-label" aria-hidden="true">
              <Trans>Protocol</Trans>
            </span>
            <SegmentedControl
              label={t`Protocol`}
              value={udp ? "udp" : "tcp"}
              options={[
                { value: "tcp", label: "TCP" },
                { value: "udp", label: "UDP" },
              ]}
              onChange={(value) => setUdp(value === "udp")}
            />
          </div>
          <div className="app-field">
            <span className="dw-field-label" aria-hidden="true">
              <Trans>Reachable from</Trans>
            </span>
            <SegmentedControl<Reach>
              label={t`Reachable from`}
              value={reach}
              options={[
                { value: "anyone", label: t`Anywhere` },
                { value: "server", label: t`Only the server` },
              ]}
              onChange={setReach}
            />
          </div>
          <CopyAsCommand
            command={methodCommand(ServiceService.method.addServicePort, {
              ...request,
              protocol: udp ? Protocol.UDP : Protocol.UNSPECIFIED,
            })}
          />
        </Advanced>
      </form>
    </Sheet>
  );
}
