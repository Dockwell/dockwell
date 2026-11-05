// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeService, ChangeState, DomainService, type ServiceInfo } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  PHONE_QUERY,
  SelectField,
  Sheet,
  Switch,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useId, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { appOf, environmentOf, nameOf } from "./app";
import { useTunnelSwitchName } from "./DomainNote";
import { formRefusal, proxyList, refusedField, routeName } from "./domains";
import { validPort } from "./form";

/** What ends TLS in front of this server, which says who the route answers. */
type InFront = "tunnel" | "cloudflare" | "proxy" | "anyone";

/**
 * Sends a domain to this app: a domain and the port it listens on, then the plan,
 * which says what a server's first route does to its ports 80 and 443. How the
 * domain gets its certificate is the server's to work out, unless a tunnel or a
 * proxy in front handles HTTPS, when there's none to get, and you say what's in front,
 * as that's who the route answers and who the app believes on who a visitor is. An app
 * on the server's own network can't be dialled by name, so the route names it and dials
 * its port on the server. The plan says what each of those means.
 */
export function AddDomainSheet({
  service,
  onClose,
  start,
}: {
  service: ServiceInfo;
  onClose: () => void;
  /** A domain that needs moving behind a tunnel, as it's refused a certificate. */
  start?: { domain: string; port: string };
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const name = nameOf(service);
  const environment = environmentOf(service);
  const known = useQuery(DomainService.method.listDomains, { environment });
  const app = appOf(service);
  const onServer = app?.hostNetwork ?? false;
  const published = app?.ports[0]?.containerPort;
  const servers = service.deployments;
  const [server, setServer] = useState(servers[0]?.serverId ?? "");
  const [domain, setDomain] = useState(start?.domain ?? "");
  // On the server's own network, there's no port it's likely to be: 80 is the proxy's.
  const [port, setPort] = useState(start?.port ?? (onServer ? "" : published ? String(published) : "80"));
  const [noCertificate, setNoCertificate] = useState(start !== undefined);
  const [inFront, setInFront] = useState<InFront>("tunnel");
  const [proxies, setProxies] = useState("");
  const [proxiesError, setProxiesError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [domainError, setDomainError] = useState<string>();
  const [portError, setPortError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLInputElement>(null);
  const form = useRef<HTMLFormElement>(null);
  const switchName = useTunnelSwitchName();
  const choiceName = useId();

  /** Takes you to the first field that was refused, ready to change, as New app does. */
  const revealError = () =>
    requestAnimationFrame(() => {
      const refused = form.current?.querySelector<HTMLInputElement>('[aria-invalid="true"]');
      refused?.focus();
      refused?.select();
    });

  // Back from a plan that failed, what it said is at the field, ready to change.
  const reveal = useRef(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs when the form comes back, and revealError is new each time.
  useEffect(() => {
    if (plan || !reveal.current) return;
    reveal.current = false;
    revealError();
  }, [plan]);

  /** Puts what the server refused at the field it's about, or above them all if it's about none of them. */
  const land = (raw: string, wanted: string) => {
    const said = formRefusal(raw, wanted);
    switch (refusedField(said, { domain: wanted, port, onServer })) {
      case "proxies":
        if (noCertificate && inFront === "proxy") setProxiesError(said);
        else setFailure(said);
        break;
      case "port":
        setPortError(said);
        break;
      case "domain":
        setDomainError(said);
        break;
      default:
        setFailure(said);
    }
  };

  // What a choice warns of is what it's for, so it isn't left below the fold of a phone.
  const said = useRef<HTMLDivElement>(null);
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs as the choice changes, once what it says is there.
  useEffect(() => {
    said.current?.scrollIntoView?.({ block: "nearest" });
  }, [inFront]);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    const wanted = domain.trim().toLowerCase();
    const domainOk = wanted !== "" && !/\s/.test(wanted);
    const portOk = validPort(port);
    const named = proxyList(proxies);
    const proxiesOk = !noCertificate || inFront !== "proxy" || named.length > 0;
    setDomainError(domainOk ? undefined : t`Name the domain, such as app.example.com.`);
    setPortError(portOk ? undefined : t`Use a port from 1 to 65535.`);
    setProxiesError(proxiesOk ? undefined : t`Name what connects to this server, as an address or a range.`);
    setFailure(undefined);
    if (!domainOk || !portOk || !proxiesOk) {
      revealError();
      return;
    }
    const front = () => {
      switch (inFront) {
        case "tunnel":
          return { tls: "off", trustedProxies: ["127.0.0.1"] };
        case "cloudflare":
          return { tls: "off", trustedProxies: ["cloudflare"] };
        case "proxy":
          return { tls: "off", trustedProxies: named };
        case "anyone":
          return { tls: "off", plainForAnyone: true };
      }
    };
    setBusy(true);
    let change: Change | undefined;
    try {
      change = (
        await client.proposeChange({
          summary: t`Send ${wanted} to ${name}`,
          edits: [
            {
              resource: { kind: "route", scope: environment, name: routeName(wanted, known.data?.domains) },
              spec: {
                kind: {
                  case: "route",
                  value: {
                    domain: wanted,
                    service: name,
                    ...(onServer ? { hostPort: Number(port) } : { port: Number(port) }),
                    ...(noCertificate ? front() : {}),
                  },
                },
              },
              ...(server ? { servers: [server] } : {}),
            },
          ],
        })
      ).change;
    } catch (err) {
      const said = sentence(err);
      // A server's agent refusing the route is about the route. What else fails isn't.
      if (reasonOf(err) === "invalid" || /^can't plan on /i.test(said)) land(said, wanted);
      else setFailure(said);
      setBusy(false);
      revealError();
      return;
    }
    setBusy(false);
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Edit and try again`,
      onDone: onClose,
      onClose: (after) => {
        // Cancelling comes back here, with what you typed, as failing does, saying why
        // at the field. Anything else is under way or over.
        if (after?.state === ChangeState.FAILED) {
          land(after.error, wanted);
          reveal.current = true;
          setPlan(undefined);
        } else if (!after || after.state === ChangeState.PLANNED) setPlan(undefined);
        else onClose();
      },
    });
  };

  const choices: { value: InFront; label: string; hint: string }[] = [
    {
      value: "tunnel",
      label: t`A tunnel on this server`,
      hint: t`Such as cloudflared on the server's own network. Your app reads the visitor's address and https. One on a bridge is another proxy here, named by that network's range.`,
    },
    { value: "cloudflare", label: t`Cloudflare's proxy`, hint: t`Cloudflare connects to this server's port 80.` },
    { value: "proxy", label: t`Another proxy`, hint: t`You name what connects to this server.` },
    {
      value: "anyone",
      label: t`Anyone who can reach this server`,
      hint: t`For a private network with nothing in front. Without this, a request to the server's own address gets a 404.`,
    },
  ];

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={t`Add a domain to ${name}`}
      subtitle={
        !noCertificate
          ? t`People reach the app at this name, over HTTPS`
          : inFront === "anyone"
            ? t`People reach the app at this name, over plain HTTP`
            : t`People reach the app at this name, through what's in front of this server`
      }
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
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void submit()}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        ref={form}
        className="app-form"
        onSubmit={(event) => void submit(event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void submit(event);
        }}
      >
        {/* Lets Enter in a field do what the button in the sheet's footer does. */}
        <button type="submit" hidden />
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        <TextField
          ref={field}
          label={t`Domain`}
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          value={domain}
          error={domainError}
          hint={
            !noCertificate
              ? t`Point its DNS at this server, before or after. Its certificate comes by itself once it does.`
              : inFront === "anyone"
                ? t`Point its DNS at this server.`
                : t`Point its DNS at your tunnel or proxy.`
          }
          onChange={(event) => {
            setDomain(event.target.value);
            setDomainError(undefined);
          }}
        />
        <TextField
          label={onServer ? t`Port on the server` : t`Port in the app`}
          mono
          inputMode="numeric"
          value={port}
          error={portError}
          hint={
            onServer
              ? t`It's on the server's own network, so what it listens on there, such as 8080. Port 80 is the proxy's.`
              : t`What the app listens on, such as 80.`
          }
          onChange={(event) => {
            setPort(event.target.value);
            setPortError(undefined);
          }}
        />
        {servers.length > 1 ? (
          <SelectField
            label={t`Server`}
            hint={t`A route runs on one server, as its certificate comes to the one its DNS points at.`}
            value={server}
            options={servers.map((d) => ({ value: d.serverId, label: d.serverName }))}
            onChange={(event) => setServer(event.target.value)}
          />
        ) : null}
        <Switch
          className="app-front-switch"
          label={switchName}
          description={t`This server takes no certificate, and traffic reaches it unencrypted.`}
          checked={noCertificate}
          onChange={setNoCertificate}
        />
        {noCertificate ? (
          <fieldset className="app-front">
            <legend className="dw-field-label">
              <Trans>What's in front</Trans>
            </legend>
            {choices.map((choice) => (
              <label key={choice.value} className={cx("app-front-choice", inFront === choice.value && "is-on")}>
                <input
                  type="radio"
                  name={choiceName}
                  checked={inFront === choice.value}
                  onChange={() => {
                    setInFront(choice.value);
                    setProxiesError(undefined);
                  }}
                />
                <span className="app-front-text">
                  <span className="app-front-name">{choice.label}</span>
                  <span className="app-front-hint">{choice.hint}</span>
                </span>
              </label>
            ))}
            <div ref={said} className="app-front-says">
              {inFront === "proxy" ? (
                <TextField
                  label={t`What connects to this server`}
                  mono
                  autoCapitalize="none"
                  autoCorrect="off"
                  spellCheck={false}
                  value={proxies}
                  error={proxiesError}
                  hint={t`An address or a range, such as 172.20.0.0/16 for a tunnel on a bridge. That proxy needs a network of its own, not Docker's default bridge. Separate several with commas.`}
                  onChange={(event) => {
                    setProxies(event.target.value);
                    setProxiesError(undefined);
                  }}
                />
              ) : null}
              {inFront === "cloudflare" ? (
                <Callout tone="warning">
                  <Trans>
                    This path crosses the open internet unencrypted, between Cloudflare and this server. It needs
                    Flexible mode in Cloudflare, and a tunnel is the better way.
                  </Trans>
                </Callout>
              ) : null}
              {inFront === "anyone" ? (
                <Callout tone="warning">
                  <Trans>
                    It answers anyone who can reach port 80 over plain HTTP, around whatever is in front. You approve
                    this in the plan.
                  </Trans>
                </Callout>
              ) : null}
            </div>
          </fieldset>
        ) : null}
      </form>
    </Sheet>
  );
}
